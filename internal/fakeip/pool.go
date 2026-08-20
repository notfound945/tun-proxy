// Package fakeip owns concurrency-safe domain to Fake IP mappings.
package fakeip

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hailinpan/tun-proxy/internal/domainname"
)

var (
	ErrExhausted = errors.New("Fake IP pool exhausted")
	ErrNotFound  = errors.New("Fake IP mapping not found")
)

type entry struct {
	domain     string
	address    netip.Addr
	expiresAt  time.Time
	references uint64
}

type Mapping struct {
	Domain    string    `yaml:"domain"`
	Address   string    `yaml:"address"`
	ExpiresAt time.Time `yaml:"expires_at"`
}

type Snapshot struct {
	Version      int       `yaml:"version"`
	Prefix       string    `yaml:"prefix"`
	SavedAt      time.Time `yaml:"saved_at"`
	JournalEpoch string    `yaml:"journal_epoch,omitempty"`
	Mappings     []Mapping `yaml:"mappings"`
}

type Pool struct {
	mutex          sync.Mutex
	prefix         netip.Prefix
	mappingTTL     time.Duration
	start          netip.Addr
	end            netip.Addr
	next           netip.Addr
	capacity       uint64
	maxMappings    int
	byDomain       map[string]*entry
	byAddress      map[netip.Addr]*entry
	pendingDeletes map[string]struct{}
	now            func() time.Time
	persist        func(persistenceUpdate, func() Snapshot) error
	flush          func(Snapshot) error
	reclaimed      uint64
	exhaustions    uint64
}

type Stats struct {
	Capacity    uint64
	Limit       int
	Used        int
	Active      int
	References  uint64
	Reclaimed   uint64
	Exhaustions uint64
}

// Prefix returns the immutable address space owned by this pool.
func (pool *Pool) Prefix() netip.Prefix {
	if pool == nil {
		return netip.Prefix{}
	}
	return pool.prefix
}

// New creates a pool, reserving reserveFirst addresses after the network
// boundary and always reserving the final address. A default /15 pool uses
// reserveFirst=10 so generated addresses start at 198.18.0.10.
func New(prefix netip.Prefix, mappingTTL time.Duration, maxMappings, reserveFirst int) (*Pool, error) {
	return newPool(prefix, mappingTTL, maxMappings, reserveFirst, time.Now)
}

func newPool(prefix netip.Prefix, mappingTTL time.Duration, maxMappings, reserveFirst int, now func() time.Time) (*Pool, error) {
	if !prefix.IsValid() || prefix.Addr().Is4In6() {
		return nil, fmt.Errorf("Fake IP pool requires an IPv4 or IPv6 prefix, got %s", prefix)
	}
	if mappingTTL <= 0 {
		return nil, errors.New("Fake IP mapping TTL must be positive")
	}
	if maxMappings <= 0 {
		return nil, errors.New("Fake IP maximum mappings must be positive")
	}
	if reserveFirst < 1 {
		return nil, errors.New("Fake IP pool must reserve at least its network address")
	}
	prefix = prefix.Masked()
	capacity, err := usableCapacity(prefix, reserveFirst)
	if err != nil {
		return nil, fmt.Errorf("Fake IP prefix %s has no capacity after reservations", prefix)
	}
	if uint64(maxMappings) > capacity {
		return nil, fmt.Errorf("Fake IP maximum mappings %d exceeds usable capacity %d", maxMappings, capacity)
	}
	start := prefix.Addr()
	for range reserveFirst {
		start = start.Next()
	}
	end := lastAddress(prefix) // exclusive; the final address is reserved
	return &Pool{
		prefix:         prefix,
		mappingTTL:     mappingTTL,
		start:          start,
		end:            end,
		next:           start,
		capacity:       capacity,
		maxMappings:    maxMappings,
		byDomain:       make(map[string]*entry, maxMappings),
		byAddress:      make(map[netip.Addr]*entry, maxMappings),
		pendingDeletes: make(map[string]struct{}),
		now:            now,
	}, nil
}

// GetOrAllocate returns one stable Fake IP for a normalized domain and renews
// its mapping lifetime. Allocation and both map updates are one mutex-protected
// operation, so concurrent queries cannot allocate duplicates.
func (pool *Pool) GetOrAllocate(domain string) (netip.Addr, error) {
	normalized, err := domainname.Normalize(domain)
	if err != nil {
		return netip.Addr{}, err
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	now := pool.now()
	if existing := pool.byDomain[normalized]; existing != nil {
		existing.expiresAt = now.Add(pool.mappingTTL)
		return existing.address, nil
	}
	pool.pruneLocked(now)
	if len(pool.byDomain) >= pool.maxMappings {
		pool.exhaustions++
		return netip.Addr{}, ErrExhausted
	}

	// At most len(byAddress) candidates can be occupied. Inspecting one more
	// distinct address therefore finds a free slot without iterating a huge
	// IPv6 prefix.
	attempts := uint64(len(pool.byAddress) + 1)
	if attempts > pool.capacity {
		attempts = pool.capacity
	}
	for range attempts {
		candidate := pool.next
		pool.next = candidate.Next()
		if !pool.next.IsValid() || pool.next == pool.end {
			pool.next = pool.start
		}
		if _, occupied := pool.byAddress[candidate]; occupied {
			continue
		}
		mapping := &entry{domain: normalized, address: candidate, expiresAt: now.Add(pool.mappingTTL)}
		pool.byDomain[normalized] = mapping
		pool.byAddress[candidate] = mapping
		if pool.persist != nil {
			removed := make([]string, 0, len(pool.pendingDeletes))
			for domain := range pool.pendingDeletes {
				removed = append(removed, domain)
			}
			slices.Sort(removed)
			update := persistenceUpdate{
				RecordedAt: now,
				Removed:    removed,
				Mapping:    Mapping{Domain: mapping.domain, Address: mapping.address.String(), ExpiresAt: mapping.expiresAt},
			}
			if err := pool.persist(update, func() Snapshot { return pool.snapshotLocked(now) }); err != nil {
				pool.deleteUntrackedLocked(mapping)
				return netip.Addr{}, fmt.Errorf("persist Fake IP allocation: %w", err)
			}
			clear(pool.pendingDeletes)
		}
		return candidate, nil
	}
	pool.exhaustions++
	return netip.Addr{}, ErrExhausted
}

func (pool *Pool) Lookup(address netip.Addr) (string, bool) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	mapping := pool.byAddress[address]
	if mapping == nil {
		return "", false
	}
	if !pool.now().Before(mapping.expiresAt) && mapping.references == 0 {
		pool.deleteLocked(mapping)
		return "", false
	}
	return mapping.domain, true
}

// Acquire holds a mapping for one active flow. The returned release function
// is idempotent and must be called when the TCP/UDP session ends.
func (pool *Pool) Acquire(address netip.Addr) (domain string, release func(), err error) {
	pool.mutex.Lock()
	mapping := pool.byAddress[address]
	if mapping == nil {
		pool.mutex.Unlock()
		return "", nil, ErrNotFound
	}
	if !pool.now().Before(mapping.expiresAt) && mapping.references == 0 {
		pool.deleteLocked(mapping)
		pool.mutex.Unlock()
		return "", nil, ErrNotFound
	}
	mapping.references++
	domain = mapping.domain
	pool.mutex.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			pool.mutex.Lock()
			defer pool.mutex.Unlock()
			if mapping.references > 0 {
				mapping.references--
			}
		})
	}
	return domain, release, nil
}

func (pool *Pool) Prune() int {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	before := len(pool.byDomain)
	pool.pruneLocked(pool.now())
	return before - len(pool.byDomain)
}

func (pool *Pool) Stats() Stats {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	stats := Stats{
		Capacity:    pool.capacity,
		Limit:       pool.maxMappings,
		Used:        len(pool.byDomain),
		Reclaimed:   pool.reclaimed,
		Exhaustions: pool.exhaustions,
	}
	for _, mapping := range pool.byDomain {
		if mapping.references > 0 {
			stats.Active++
			stats.References += mapping.references
		}
	}
	return stats
}

func (pool *Pool) Snapshot() Snapshot {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	return pool.snapshotLocked(pool.now())
}

func (pool *Pool) snapshotLocked(now time.Time) Snapshot {
	mappings := make([]Mapping, 0, len(pool.byDomain))
	for _, mapping := range pool.byDomain {
		mappings = append(mappings, Mapping{
			Domain: mapping.domain, Address: mapping.address.String(), ExpiresAt: mapping.expiresAt,
		})
	}
	slices.SortFunc(mappings, func(left, right Mapping) int {
		return strings.Compare(left.Domain, right.Domain)
	})
	return Snapshot{Version: persistenceVersion, Prefix: pool.prefix.String(), SavedAt: now, Mappings: mappings}
}

func (pool *Pool) restore(snapshot Snapshot, protectionWindow time.Duration) error {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if snapshot.Version != persistenceVersion {
		return fmt.Errorf("persistence version must be %d, got %d", persistenceVersion, snapshot.Version)
	}
	if snapshot.Prefix != pool.prefix.String() {
		return fmt.Errorf("persistence prefix is %q, want %q", snapshot.Prefix, pool.prefix)
	}
	if snapshot.JournalEpoch != "" && !validJournalEpoch(snapshot.JournalEpoch) {
		return errors.New("persistence journal epoch is invalid")
	}
	if len(snapshot.Mappings) > pool.maxMappings {
		return fmt.Errorf("persistence contains %d mappings, limit is %d", len(snapshot.Mappings), pool.maxMappings)
	}
	protectedUntil := pool.now().Add(protectionWindow)
	byDomain := make(map[string]*entry, len(snapshot.Mappings))
	byAddress := make(map[netip.Addr]*entry, len(snapshot.Mappings))
	for index, item := range snapshot.Mappings {
		domain, address, expiresAt, err := pool.validateMapping(item, protectedUntil, fmt.Sprintf("mapping %d", index))
		if err != nil {
			return err
		}
		if _, exists := byDomain[domain]; exists {
			return fmt.Errorf("mapping %d duplicates domain %q", index, domain)
		}
		if previous := byAddress[address]; previous != nil {
			return fmt.Errorf("mapping %d address %s is already assigned to %q", index, address, previous.domain)
		}
		mapping := &entry{domain: domain, address: address, expiresAt: expiresAt}
		byDomain[domain] = mapping
		byAddress[address] = mapping
	}
	pool.byDomain = byDomain
	pool.byAddress = byAddress
	return nil
}

func (pool *Pool) restoreJournal(updates []persistenceUpdate, protectionWindow time.Duration) error {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	protectedUntil := pool.now().Add(protectionWindow)
	byDomain := make(map[string]*entry, len(pool.byDomain)+len(updates))
	byAddress := make(map[netip.Addr]*entry, len(pool.byAddress)+len(updates))
	for domain, mapping := range pool.byDomain {
		byDomain[domain] = mapping
	}
	for address, mapping := range pool.byAddress {
		byAddress[address] = mapping
	}
	for index, update := range updates {
		for removedIndex, removed := range update.Removed {
			domain, err := domainname.Normalize(removed)
			if err != nil {
				return fmt.Errorf("journal record %d removed domain %d: %w", index, removedIndex, err)
			}
			if previous := byDomain[domain]; previous != nil {
				delete(byAddress, previous.address)
				delete(byDomain, domain)
			}
		}
		item := update.Mapping
		domain, address, expiresAt, err := pool.validateMapping(item, protectedUntil, fmt.Sprintf("journal record %d", index))
		if err != nil {
			return err
		}
		if previous := byDomain[domain]; previous != nil {
			delete(byAddress, previous.address)
			delete(byDomain, domain)
		}
		if previous := byAddress[address]; previous != nil {
			delete(byDomain, previous.domain)
			delete(byAddress, address)
		}
		if len(byDomain) >= pool.maxMappings {
			return fmt.Errorf("journal record %d exceeds mapping limit %d", index, pool.maxMappings)
		}
		mapping := &entry{domain: domain, address: address, expiresAt: expiresAt}
		byDomain[domain] = mapping
		byAddress[address] = mapping
	}
	pool.byDomain = byDomain
	pool.byAddress = byAddress
	return nil
}

func (pool *Pool) validateMapping(item Mapping, protectedUntil time.Time, label string) (string, netip.Addr, time.Time, error) {
	domain, err := domainname.Normalize(item.Domain)
	if err != nil {
		return "", netip.Addr{}, time.Time{}, fmt.Errorf("%s domain: %w", label, err)
	}
	address, err := netip.ParseAddr(item.Address)
	if err != nil || address.BitLen() != pool.prefix.Addr().BitLen() || address.Is4In6() {
		return "", netip.Addr{}, time.Time{}, fmt.Errorf("%s has invalid %s address %q", label, addressFamily(pool.prefix.Addr()), item.Address)
	}
	if address.Compare(pool.start) < 0 || address.Compare(pool.end) >= 0 || !pool.prefix.Contains(address) {
		return "", netip.Addr{}, time.Time{}, fmt.Errorf("%s address %s is outside usable prefix", label, address)
	}
	if item.ExpiresAt.IsZero() {
		return "", netip.Addr{}, time.Time{}, fmt.Errorf("%s expiration is required", label)
	}
	expiresAt := item.ExpiresAt
	if expiresAt.Before(protectedUntil) {
		expiresAt = protectedUntil
	}
	return domain, address, expiresAt, nil
}

func (pool *Pool) setPersistence(persist func(persistenceUpdate, func() Snapshot) error, flush func(Snapshot) error) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	pool.persist = persist
	pool.flush = flush
}

func (pool *Pool) flushPersistence() error {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if pool.flush == nil {
		return nil
	}
	if err := pool.flush(pool.snapshotLocked(pool.now())); err != nil {
		return err
	}
	clear(pool.pendingDeletes)
	return nil
}

func (pool *Pool) pruneLocked(now time.Time) {
	for _, mapping := range pool.byDomain {
		if !now.Before(mapping.expiresAt) && mapping.references == 0 {
			pool.deleteLocked(mapping)
			pool.reclaimed++
		}
	}
}

func (pool *Pool) deleteLocked(mapping *entry) {
	if pool.persist != nil {
		pool.pendingDeletes[mapping.domain] = struct{}{}
	}
	pool.deleteUntrackedLocked(mapping)
}

func (pool *Pool) deleteUntrackedLocked(mapping *entry) {
	delete(pool.byDomain, mapping.domain)
	delete(pool.byAddress, mapping.address)
}

func usableCapacity(prefix netip.Prefix, reserveFirst int) (uint64, error) {
	hostBits := prefix.Addr().BitLen() - prefix.Bits()
	reserved := uint64(reserveFirst) + 1
	if hostBits > 64 {
		return math.MaxUint64, nil
	}
	if hostBits == 64 {
		return math.MaxUint64 - uint64(reserveFirst), nil
	}
	total := uint64(1) << uint(hostBits)
	if total <= reserved {
		return 0, errors.New("reservations consume prefix")
	}
	return total - reserved, nil
}

func lastAddress(prefix netip.Prefix) netip.Addr {
	if prefix.Addr().Is4() {
		bytes := prefix.Addr().As4()
		setHostBits(bytes[:], prefix.Bits())
		return netip.AddrFrom4(bytes)
	}
	bytes := prefix.Addr().As16()
	setHostBits(bytes[:], prefix.Bits())
	return netip.AddrFrom16(bytes)
}

func setHostBits(address []byte, prefixBits int) {
	for bit := prefixBits; bit < len(address)*8; bit++ {
		address[bit/8] |= 1 << uint(7-bit%8)
	}
}

func addressFamily(address netip.Addr) string {
	if address.Is4() {
		return "IPv4"
	}
	return "IPv6"
}
