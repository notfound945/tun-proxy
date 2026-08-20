package fakeip

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistenceRestoresStableMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	now := time.Unix(1_000, 0).UTC()
	first, err := newPool(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	persistence, quarantined, err := OpenPersistence(path, first, time.Minute)
	if err != nil || quarantined != "" {
		t.Fatalf("OpenPersistence() = (%q, %v)", quarantined, err)
	}
	address, err := first.GetOrAllocate("Example.COM.")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("persistence mode = %v", info.Mode().Perm())
	}
	if err := persistence.Flush(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)
	second, err := newPool(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err = OpenPersistence(path, second, 5*time.Minute)
	if err != nil || quarantined != "" {
		t.Fatalf("restore = (%q, %v)", quarantined, err)
	}
	if domain, ok := second.Lookup(address); !ok || domain != "example.com" {
		t.Fatalf("restored lookup = (%q, %t)", domain, ok)
	}
	now = now.Add(4 * time.Minute)
	if second.Prune() != 0 {
		t.Fatal("mapping was reclaimed inside restart protection window")
	}
	now = now.Add(2 * time.Minute)
	if second.Prune() != 1 {
		t.Fatal("mapping was not reclaimed after restart protection window")
	}
}

func TestPersistenceReplaysAllocationsWithoutSnapshotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	now := time.Unix(3_000, 0).UTC()
	first, err := newPool(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenPersistence(path, first, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	initial, exists, err := readSnapshot(path)
	if err != nil || !exists || len(initial.Mappings) != 0 {
		t.Fatalf("initial snapshot = (%+v, %t, %v)", initial, exists, err)
	}
	addresses := make(map[string]netip.Addr)
	for _, domain := range []string{"one.example", "two.example", "three.example"} {
		address, err := first.GetOrAllocate(domain)
		if err != nil {
			t.Fatal(err)
		}
		addresses[domain] = address
	}
	unchanged, exists, err := readSnapshot(path)
	if err != nil || !exists || len(unchanged.Mappings) != 0 || unchanged.JournalEpoch != initial.JournalEpoch {
		t.Fatalf("allocation rewrote snapshot = (%+v, %t, %v)", unchanged, exists, err)
	}
	records, exists, err := readJournal(path + ".wal")
	if err != nil || !exists || len(records) != len(addresses) {
		t.Fatalf("journal = (%d records, %t, %v)", len(records), exists, err)
	}

	second, err := newPool(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, second, time.Minute)
	if err != nil || quarantined != "" {
		t.Fatalf("reopen = (%q, %v)", quarantined, err)
	}
	for domain, address := range addresses {
		if restored, ok := second.Lookup(address); !ok || restored != domain {
			t.Fatalf("restored %s = (%q, %t)", address, restored, ok)
		}
	}
}

func TestPersistenceDoesNotReplayJournalWithoutValidSnapshot(t *testing.T) {
	for _, test := range []struct {
		name       string
		invalidate func(*testing.T, string)
	}{
		{
			name: "missing",
			invalidate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt",
			invalidate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("version: [broken\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fake-ip.yaml")
			now := time.Unix(3_500, 0).UTC()
			first, err := newPool(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			persistence, _, err := OpenPersistence(path, first, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			snapshotAddress, err := first.GetOrAllocate("snapshot.example")
			if err != nil {
				t.Fatal(err)
			}
			if err := persistence.Flush(); err != nil {
				t.Fatal(err)
			}
			journalAddress, err := first.GetOrAllocate("journal.example")
			if err != nil {
				t.Fatal(err)
			}
			if snapshotAddress == journalAddress {
				t.Fatalf("snapshot and journal mappings share address %s", snapshotAddress)
			}

			test.invalidate(t, path)

			restored, err := newPool(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			_, quarantined, err := OpenPersistence(path, restored, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(quarantined, ".wal.corrupt-") {
				t.Fatalf("journal was not quarantined: %q", quarantined)
			}
			if _, ok := restored.Lookup(snapshotAddress); ok {
				t.Fatal("mapping from invalid snapshot was restored")
			}
			if _, ok := restored.Lookup(journalAddress); ok {
				t.Fatal("incremental journal was replayed without its base snapshot")
			}
			if snapshot, exists, err := readSnapshot(path); err != nil || !exists || len(snapshot.Mappings) != 0 {
				t.Fatalf("replacement snapshot = (%+v, %t, %v)", snapshot, exists, err)
			}
			if _, exists, err := readJournal(path + ".wal"); err != nil || exists {
				t.Fatalf("replacement journal = (exists=%t, err=%v)", exists, err)
			}
			matches, err := filepath.Glob(path + ".wal.corrupt-*")
			if err != nil || len(matches) != 1 {
				t.Fatalf("quarantined journals = (%v, %v)", matches, err)
			}
		})
	}
}

func TestPersistenceJournalRecordsPrunedMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	now := time.Unix(4_000, 0).UTC()
	first, err := newPool(netip.MustParsePrefix("198.18.0.0/29"), time.Minute, 1, 1, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	persistence, _, err := OpenPersistence(path, first, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	oldAddress, err := first.GetOrAllocate("old.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Flush(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if first.Prune() != 1 {
		t.Fatal("expired mapping was not pruned")
	}
	newAddress, err := first.GetOrAllocate("new.example")
	if err != nil {
		t.Fatal(err)
	}

	second, err := newPool(netip.MustParsePrefix("198.18.0.0/29"), time.Minute, 1, 1, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, second, time.Minute)
	if err != nil || quarantined != "" {
		t.Fatalf("reopen = (%q, %v)", quarantined, err)
	}
	if _, ok := second.Lookup(oldAddress); ok {
		t.Fatal("pruned mapping was restored from the old snapshot")
	}
	if domain, ok := second.Lookup(newAddress); !ok || domain != "new.example" {
		t.Fatalf("new mapping = (%q, %t)", domain, ok)
	}
}

func TestIPv6PersistenceRestoresStableMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ipv6.yaml")
	now := time.Unix(2_000, 0).UTC()
	first, err := newPool(netip.MustParsePrefix("fd00:7::/120"), time.Hour, 32, 10, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	persistence, quarantined, err := OpenPersistence(path, first, time.Minute)
	if err != nil || quarantined != "" {
		t.Fatalf("OpenPersistence() = (%q, %v)", quarantined, err)
	}
	address, err := first.GetOrAllocate("Example.COM.")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Flush(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)
	second, err := newPool(netip.MustParsePrefix("fd00:7::/120"), time.Hour, 32, 10, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err = OpenPersistence(path, second, 5*time.Minute)
	if err != nil || quarantined != "" {
		t.Fatalf("restore = (%q, %v)", quarantined, err)
	}
	if domain, ok := second.Lookup(address); !ok || domain != "example.com" {
		t.Fatalf("restored lookup = (%q, %t)", domain, ok)
	}
}

func TestPersistenceRejectsAddressFromOtherFamily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ipv6.yaml")
	contents := `version: 1
prefix: fd00:7::/120
saved_at: 2026-08-18T00:00:00Z
mappings:
  - domain: wrong-family.example
    address: 198.18.0.10
    expires_at: 2026-08-19T00:00:00Z
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := New(netip.MustParsePrefix("fd00:7::/120"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, pool, time.Minute)
	if err != nil || quarantined == "" {
		t.Fatalf("OpenPersistence() = (%q, %v)", quarantined, err)
	}
}

func TestPersistenceFailureRollsBackAllocation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fake-ip.yaml")
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenPersistence(path, pool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.GetOrAllocate("example.com"); err == nil {
		t.Fatal("allocation succeeded without durable persistence")
	}
	if pool.Stats().Used != 0 {
		t.Fatalf("failed allocation remained visible: %+v", pool.Stats())
	}
}

func TestCorruptPersistenceIsQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	if err := os.WriteFile(path, []byte("version: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, pool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(quarantined, ".corrupt-") {
		t.Fatalf("quarantine path = %q", quarantined)
	}
	if _, err := os.Stat(quarantined); err != nil {
		t.Fatalf("quarantined file: %v", err)
	}
	if snapshot, exists, err := readSnapshot(path); err != nil || !exists || len(snapshot.Mappings) != 0 {
		t.Fatalf("replacement snapshot = (%+v, %t, %v)", snapshot, exists, err)
	}
}

func TestCorruptJournalIsQuarantinedWithoutLosingSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	persistence, _, err := OpenPersistence(path, pool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	address, err := pool.GetOrAllocate("snapshot.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Flush(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(path+".wal", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString("incomplete journal record"); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(journal.Sync(), journal.Close()); err != nil {
		t.Fatal(err)
	}

	restored, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, restored, time.Minute)
	if err != nil || !strings.Contains(quarantined, ".wal.corrupt-") {
		t.Fatalf("OpenPersistence() = (%q, %v)", quarantined, err)
	}
	if domain, ok := restored.Lookup(address); !ok || domain != "snapshot.example" {
		t.Fatalf("snapshot mapping after journal quarantine = (%q, %t)", domain, ok)
	}
}

func TestTornJournalRetainsDurableRecordPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenPersistence(path, pool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	address, err := pool.GetOrAllocate("durable.example")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(path+".wal", os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString("partial"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, restored, time.Minute)
	if err != nil || !strings.Contains(quarantined, ".wal.corrupt-") {
		t.Fatalf("OpenPersistence() = (%q, %v)", quarantined, err)
	}
	if domain, ok := restored.Lookup(address); !ok || domain != "durable.example" {
		t.Fatalf("durable journal prefix = (%q, %t)", domain, ok)
	}
	if _, exists, err := readJournal(path + ".wal"); err != nil || exists {
		t.Fatalf("compacted replacement journal = (exists=%t, err=%v)", exists, err)
	}
}

func TestInvalidDuplicatePersistenceIsQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-ip.yaml")
	contents := `version: 1
prefix: 198.18.0.0/24
saved_at: 2026-08-18T00:00:00Z
mappings:
  - domain: one.example
    address: 198.18.0.10
    expires_at: 2026-08-19T00:00:00Z
  - domain: two.example
    address: 198.18.0.10
    expires_at: 2026-08-19T00:00:00Z
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, quarantined, err := OpenPersistence(path, pool, time.Minute)
	if err != nil || quarantined == "" {
		t.Fatalf("OpenPersistence() = (%q, %v)", quarantined, err)
	}
}
