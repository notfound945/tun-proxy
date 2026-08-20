package fakeip

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestStableDistinctBidirectionalMappings(t *testing.T) {
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.GetOrAllocate("Example.COM.")
	if err != nil {
		t.Fatal(err)
	}
	again, err := pool.GetOrAllocate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.GetOrAllocate("other.example")
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == second || first.String() != "198.18.0.10" {
		t.Fatalf("first=%s again=%s second=%s", first, again, second)
	}
	if domain, ok := pool.Lookup(first); !ok || domain != "example.com" {
		t.Fatalf("Lookup(%s) = (%q, %t)", first, domain, ok)
	}
}

func TestIPv6StableDistinctBidirectionalMappings(t *testing.T) {
	pool, err := New(netip.MustParsePrefix("fd00:7::/120"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.GetOrAllocate("Example.COM.")
	if err != nil {
		t.Fatal(err)
	}
	again, err := pool.GetOrAllocate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.GetOrAllocate("other.example")
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == second || first.String() != "fd00:7::a" {
		t.Fatalf("first=%s again=%s second=%s", first, again, second)
	}
	if domain, ok := pool.Lookup(first); !ok || domain != "example.com" {
		t.Fatalf("Lookup(%s) = (%q, %t)", first, domain, ok)
	}
	if !first.Is6() || first.Is4In6() {
		t.Fatalf("allocated non-IPv6 address %s", first)
	}
}

func TestIPv6CapacityAndReservations(t *testing.T) {
	pool, err := New(netip.MustParsePrefix("fd00:8::/124"), time.Hour, 14, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.Stats().Capacity; got != 14 {
		t.Fatalf("capacity = %d, want 14", got)
	}
	for index := 1; index <= 14; index++ {
		address, err := pool.GetOrAllocate(fmt.Sprintf("host-%d.example", index))
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 && address.String() != "fd00:8::1" {
			t.Fatalf("first address = %s", address)
		}
	}
	if _, err := pool.GetOrAllocate("exhausted.example"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("allocation after capacity = %v", err)
	}
}

func TestIPv6LargeCapacityIsSafe(t *testing.T) {
	pool64, err := New(netip.MustParsePrefix("fd00:9::/64"), time.Hour, 65_536, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pool64.Stats().Capacity, uint64(math.MaxUint64-10); got != want {
		t.Fatalf("/64 capacity = %d, want %d", got, want)
	}
	pool48, err := New(netip.MustParsePrefix("fd00:a::/48"), time.Hour, 65_536, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := pool48.Stats().Capacity; got != math.MaxUint64 {
		t.Fatalf("/48 saturated capacity = %d", got)
	}
}

func TestConcurrentQueriesAllocateOneAddress(t *testing.T) {
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	addresses := make(chan netip.Addr, 64)
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			address, err := pool.GetOrAllocate("concurrent.example")
			if err != nil {
				t.Error(err)
				return
			}
			addresses <- address
		}()
	}
	wait.Wait()
	close(addresses)
	var first netip.Addr
	for address := range addresses {
		if !first.IsValid() {
			first = address
		} else if address != first {
			t.Fatalf("allocated %s and %s for one domain", first, address)
		}
	}
	if pool.Stats().Used != 1 {
		t.Fatalf("stats = %+v", pool.Stats())
	}
}

func TestPoolExhaustionAndReclaim(t *testing.T) {
	now := time.Unix(100, 0)
	pool, err := newPool(netip.MustParsePrefix("198.18.0.0/29"), time.Minute, 2, 1, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, _ := pool.GetOrAllocate("one.example")
	_, _ = pool.GetOrAllocate("two.example")
	if _, err := pool.GetOrAllocate("three.example"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("third allocation = %v, want ErrExhausted", err)
	}
	domain, release, err := pool.Acquire(first)
	if err != nil || domain != "one.example" {
		t.Fatalf("Acquire() = (%q, %v)", domain, err)
	}
	now = now.Add(2 * time.Minute)
	pool.Prune()
	if stats := pool.Stats(); stats.Used != 1 || stats.Active != 1 {
		t.Fatalf("held mapping was reclaimed: %+v", stats)
	}
	if _, err := pool.GetOrAllocate("three.example"); err != nil {
		t.Fatalf("allocation after reclaim: %v", err)
	}
	release()
	release()
	now = now.Add(2 * time.Minute)
	pool.Prune()
	if pool.Stats().Used != 0 {
		t.Fatalf("released mappings not reclaimed: %+v", pool.Stats())
	}
}

func TestPersistenceIODoesNotHoldPoolMutex(t *testing.T) {
	newTestPool := func(t *testing.T) *Pool {
		t.Helper()
		pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
		if err != nil {
			t.Fatal(err)
		}
		return pool
	}
	assertStatsAvailable := func(t *testing.T, pool *Pool) {
		t.Helper()
		result := make(chan Stats, 1)
		go func() { result <- pool.Stats() }()
		select {
		case stats := <-result:
			if stats.Used != 1 {
				t.Fatalf("Stats() during persistence = %+v", stats)
			}
		case <-time.After(time.Second):
			t.Fatal("Stats() blocked while persistence performed I/O")
		}
	}

	t.Run("allocation journal", func(t *testing.T) {
		pool := newTestPool(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		pool.setPersistence(func(_ persistenceUpdate, snapshot func() Snapshot) error {
			current := snapshot()
			if len(current.Mappings) != 1 {
				t.Errorf("persistence snapshot mappings = %d", len(current.Mappings))
			}
			close(entered)
			<-release
			return nil
		}, func(Snapshot) error { return nil })

		allocation := make(chan error, 1)
		go func() {
			_, err := pool.GetOrAllocate("example.com")
			allocation <- err
		}()
		<-entered
		assertStatsAvailable(t, pool)
		close(release)
		if err := <-allocation; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("snapshot flush", func(t *testing.T) {
		pool := newTestPool(t)
		pool.setPersistence(func(_ persistenceUpdate, _ func() Snapshot) error { return nil }, func(Snapshot) error { return nil })
		if _, err := pool.GetOrAllocate("example.com"); err != nil {
			t.Fatal(err)
		}

		entered := make(chan struct{})
		release := make(chan struct{})
		pool.setPersistence(func(_ persistenceUpdate, _ func() Snapshot) error { return nil }, func(snapshot Snapshot) error {
			if len(snapshot.Mappings) != 1 {
				t.Errorf("flush snapshot mappings = %d", len(snapshot.Mappings))
			}
			close(entered)
			<-release
			return nil
		})
		flushed := make(chan error, 1)
		go func() { flushed <- pool.flushPersistence() }()
		<-entered
		assertStatsAvailable(t, pool)
		close(release)
		if err := <-flushed; err != nil {
			t.Fatal(err)
		}
	})
}
