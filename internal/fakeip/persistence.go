package fakeip

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	persistenceVersion   = 1
	maxPersistenceSize   = 16 << 20
	maxJournalRecordSize = 4 << 10
)

type Persistence struct {
	path         string
	journalPath  string
	journalEpoch string
	pool         *Pool
}

type persistenceUpdate struct {
	RecordedAt time.Time
	Removed    []string
	Mapping    Mapping
}

type journalRecord struct {
	Version    int       `json:"version"`
	Prefix     string    `json:"prefix"`
	Epoch      string    `json:"epoch"`
	RecordedAt time.Time `json:"recorded_at"`
	Removed    []string  `json:"removed,omitempty"`
	Mapping    Mapping   `json:"mapping"`
}

// OpenPersistence restores a valid snapshot and makes every new allocation
// durable before it can be returned to DNS clients. Invalid snapshot contents
// are quarantined; unsafe paths and I/O failures remain fatal.
func OpenPersistence(path string, pool *Pool, protectionWindow time.Duration) (*Persistence, string, error) {
	if pool == nil {
		return nil, "", errors.New("Fake IP pool is required")
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", fmt.Errorf("persistence path must be a clean absolute path: %q", path)
	}
	if protectionWindow < 0 {
		return nil, "", errors.New("persistence protection window cannot be negative")
	}
	persistence := &Persistence{path: path, journalPath: path + ".wal", pool: pool}
	snapshot, exists, err := readSnapshot(path)
	var quarantined string
	if err != nil {
		var corrupt *corruptPersistenceError
		if !errors.As(err, &corrupt) {
			return nil, "", err
		}
		quarantined, err = quarantine(path)
		if err != nil {
			return nil, "", errors.Join(corrupt, err)
		}
		exists = false
	} else if exists {
		if restoreErr := pool.restore(snapshot, protectionWindow); restoreErr != nil {
			quarantined, err = quarantine(path)
			if err != nil {
				return nil, "", errors.Join(fmt.Errorf("invalid Fake IP persistence: %w", restoreErr), err)
			}
			exists = false
		}
	}
	records, _, journalErr := readJournal(persistence.journalPath)
	journalQuarantined := false
	journalNeedsCheckpoint := false
	if journalErr != nil {
		var corrupt *corruptPersistenceError
		if !errors.As(journalErr, &corrupt) {
			return nil, quarantined, journalErr
		}
		journalQuarantine, quarantineErr := quarantine(persistence.journalPath)
		if quarantineErr != nil {
			return nil, quarantined, errors.Join(corrupt, quarantineErr)
		}
		quarantined = joinQuarantinePaths(quarantined, journalQuarantine)
		journalQuarantined = true
		journalNeedsCheckpoint = true
	}
	active, epoch, journalErr := activeJournalRecords(records, snapshot, exists, pool.prefix.String())
	if journalErr == nil && len(active) != 0 {
		updates := make([]persistenceUpdate, len(active))
		for index := range active {
			updates[index] = persistenceUpdate{
				RecordedAt: active[index].RecordedAt,
				Removed:    append([]string(nil), active[index].Removed...), Mapping: active[index].Mapping,
			}
		}
		journalErr = pool.restoreJournal(updates, protectionWindow)
	}
	if journalErr != nil {
		if !journalQuarantined {
			journalQuarantine, quarantineErr := quarantine(persistence.journalPath)
			if quarantineErr != nil {
				return nil, quarantined, errors.Join(fmt.Errorf("invalid Fake IP journal: %w", journalErr), quarantineErr)
			}
			quarantined = joinQuarantinePaths(quarantined, journalQuarantine)
		}
		journalNeedsCheckpoint = true
		epoch = ""
	}
	if epoch == "" {
		epoch, err = newJournalEpoch()
		if err != nil {
			return nil, quarantined, err
		}
	}
	persistence.journalEpoch = epoch
	if !exists || journalNeedsCheckpoint {
		if err := persistence.checkpoint(pool.Snapshot()); err != nil {
			return nil, quarantined, fmt.Errorf("initialize Fake IP persistence: %w", err)
		}
	}
	pool.setPersistence(persistence.record, persistence.checkpoint)
	return persistence, quarantined, nil
}

func (persistence *Persistence) Flush() error {
	if persistence == nil || persistence.pool == nil {
		return nil
	}
	return persistence.pool.flushPersistence()
}

func (persistence *Persistence) record(update persistenceUpdate, snapshot func() Snapshot) error {
	record := journalRecord{
		Version: persistenceVersion, Prefix: persistence.pool.prefix.String(), Epoch: persistence.journalEpoch,
		RecordedAt: update.RecordedAt, Removed: update.Removed, Mapping: update.Mapping,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode Fake IP journal record: %w", err)
	}
	if len(encoded)+1 > maxJournalRecordSize {
		return persistence.checkpoint(snapshot())
	}
	exists, size, err := inspectJournal(persistence.journalPath)
	if err != nil {
		return err
	}
	if size+int64(len(encoded)+1) > maxPersistenceSize {
		return persistence.checkpoint(snapshot())
	}
	file, err := os.OpenFile(persistence.journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Fake IP journal: %w", err)
	}
	rollback := func() {
		_ = file.Truncate(size)
		_ = file.Sync()
	}
	if !exists {
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return fmt.Errorf("chmod Fake IP journal: %w", err)
		}
	}
	encoded = append(encoded, '\n')
	if err := writeAll(file, encoded); err != nil {
		rollback()
		_ = file.Close()
		return fmt.Errorf("append Fake IP journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		rollback()
		_ = file.Close()
		return fmt.Errorf("sync Fake IP journal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Fake IP journal: %w", err)
	}
	if !exists {
		if err := syncDirectory(filepath.Dir(persistence.journalPath)); err != nil {
			return fmt.Errorf("sync Fake IP journal directory: %w", err)
		}
	}
	return nil
}

func (persistence *Persistence) checkpoint(snapshot Snapshot) error {
	epoch, err := newJournalEpoch()
	if err != nil {
		return err
	}
	snapshot.JournalEpoch = epoch
	if err := persistence.writeSnapshot(snapshot); err != nil {
		return err
	}
	// The durable snapshot is now authoritative. Switch epochs before removing
	// the old log so a failed removal cannot make later records ambiguous.
	persistence.journalEpoch = epoch
	if err := persistence.clearJournal(); err != nil {
		return err
	}
	return nil
}

func (persistence *Persistence) writeSnapshot(snapshot Snapshot) error {
	directory := filepath.Dir(persistence.path)
	if err := validateTarget(persistence.path); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tun-proxy-fake-ip-*")
	if err != nil {
		return fmt.Errorf("create temporary persistence file: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temporary persistence file: %w", err)
	}
	encoder := yaml.NewEncoder(temporary)
	encoder.SetIndent(2)
	if err := encoder.Encode(snapshot); err != nil {
		return fmt.Errorf("encode Fake IP persistence: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("close persistence encoder: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync persistence file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close persistence file: %w", err)
	}
	if err := os.Rename(temporaryPath, persistence.path); err != nil {
		return fmt.Errorf("replace persistence file: %w", err)
	}
	keep = true
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync persistence directory: %w", err)
	}
	return nil
}

func (persistence *Persistence) clearJournal() error {
	if err := validateTarget(persistence.journalPath); err != nil {
		return err
	}
	if err := os.Remove(persistence.journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove compacted Fake IP journal: %w", err)
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := syncDirectory(filepath.Dir(persistence.journalPath)); err != nil {
		return fmt.Errorf("sync compacted Fake IP journal directory: %w", err)
	}
	return nil
}

type corruptPersistenceError struct{ err error }

func (err *corruptPersistenceError) Error() string {
	return "corrupt Fake IP persistence: " + err.err.Error()
}
func (err *corruptPersistenceError) Unwrap() error { return err.err }

func readSnapshot(path string) (Snapshot, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("inspect persistence file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Snapshot{}, false, fmt.Errorf("persistence path %q is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Snapshot{}, false, fmt.Errorf("persistence file %q permissions are %04o, want 0600 or stricter", path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return Snapshot{}, false, fmt.Errorf("persistence file %q is not owned by UID %d", path, os.Geteuid())
	}
	if info.Size() > maxPersistenceSize {
		return Snapshot{}, false, &corruptPersistenceError{fmt.Errorf("file exceeds %d bytes", maxPersistenceSize)}
	}
	file, err := os.Open(path)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("open persistence file: %w", err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(io.LimitReader(file, maxPersistenceSize+1))
	decoder.KnownFields(true)
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, false, &corruptPersistenceError{err}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing YAML document is not allowed")
		}
		return Snapshot{}, false, &corruptPersistenceError{err}
	}
	return snapshot, true, nil
}

func readJournal(path string) ([]journalRecord, bool, error) {
	exists, size, err := inspectJournal(path)
	if err != nil || !exists {
		return nil, exists, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, true, fmt.Errorf("open Fake IP journal: %w", err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxPersistenceSize+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, true, fmt.Errorf("read Fake IP journal: %w", readErr)
	}
	if closeErr != nil {
		return nil, true, fmt.Errorf("close Fake IP journal: %w", closeErr)
	}
	if int64(len(contents)) != size {
		return nil, true, &corruptPersistenceError{errors.New("journal changed while it was being read")}
	}
	if len(contents) == 0 {
		return nil, true, nil
	}
	complete := contents
	var tailErr error
	if contents[len(contents)-1] != '\n' {
		tailErr = &corruptPersistenceError{errors.New("journal ends with an incomplete record")}
		lastNewline := bytes.LastIndexByte(contents, '\n')
		if lastNewline < 0 {
			complete = nil
		} else {
			complete = contents[:lastNewline+1]
		}
	}
	if len(complete) == 0 {
		return nil, true, tailErr
	}
	lines := bytes.Split(complete[:len(complete)-1], []byte{'\n'})
	records := make([]journalRecord, 0, len(lines))
	for index, line := range lines {
		if len(line) == 0 || len(line)+1 > maxJournalRecordSize {
			return records, true, &corruptPersistenceError{fmt.Errorf("journal record %d has invalid size", index)}
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var record journalRecord
		if err := decoder.Decode(&record); err != nil {
			return records, true, &corruptPersistenceError{fmt.Errorf("decode journal record %d: %w", index, err)}
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("trailing JSON value is not allowed")
			}
			return records, true, &corruptPersistenceError{fmt.Errorf("decode journal record %d: %w", index, err)}
		}
		records = append(records, record)
	}
	return records, true, tailErr
}

func inspectJournal(path string) (bool, int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("inspect Fake IP journal: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, 0, fmt.Errorf("Fake IP journal %q is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, 0, fmt.Errorf("Fake IP journal %q permissions are %04o, want 0600 or stricter", path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return false, 0, fmt.Errorf("Fake IP journal %q is not owned by UID %d", path, os.Geteuid())
	}
	if info.Size() > maxPersistenceSize {
		return false, 0, &corruptPersistenceError{fmt.Errorf("journal exceeds %d bytes", maxPersistenceSize)}
	}
	return true, info.Size(), nil
}

func activeJournalRecords(records []journalRecord, snapshot Snapshot, snapshotExists bool, prefix string) ([]journalRecord, string, error) {
	// Journal records are deltas from a checkpoint. Replaying them without the
	// matching snapshot would restore partial state and could reuse live addresses.
	if !snapshotExists && len(records) != 0 {
		return nil, "", errors.New("Fake IP journal requires a valid snapshot")
	}
	wantedEpoch := ""
	if snapshotExists {
		wantedEpoch = snapshot.JournalEpoch
		if wantedEpoch != "" && !validJournalEpoch(wantedEpoch) {
			return nil, "", errors.New("snapshot journal epoch is invalid")
		}
	}
	active := make([]journalRecord, 0, len(records))
	for index, record := range records {
		if record.Version != persistenceVersion {
			return nil, "", fmt.Errorf("journal record %d version must be %d, got %d", index, persistenceVersion, record.Version)
		}
		if record.Prefix != prefix {
			return nil, "", fmt.Errorf("journal record %d prefix is %q, want %q", index, record.Prefix, prefix)
		}
		if !validJournalEpoch(record.Epoch) {
			return nil, "", fmt.Errorf("journal record %d epoch is invalid", index)
		}
		if record.RecordedAt.IsZero() {
			return nil, "", fmt.Errorf("journal record %d timestamp is required", index)
		}
		if wantedEpoch == "" {
			wantedEpoch = record.Epoch
		}
		if snapshotExists && snapshot.JournalEpoch != "" && record.Epoch != wantedEpoch {
			continue
		}
		if record.Epoch != wantedEpoch {
			return nil, "", fmt.Errorf("journal contains mixed epochs %q and %q", wantedEpoch, record.Epoch)
		}
		active = append(active, record)
	}
	return active, wantedEpoch, nil
}

func newJournalEpoch() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Fake IP journal epoch: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validJournalEpoch(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

func joinQuarantinePaths(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + ", " + right
}

func writeAll(writer io.Writer, contents []byte) error {
	for len(contents) != 0 {
		written, err := writer.Write(contents)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		contents = contents[written:]
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateTarget(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect persistence target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("persistence target %q is not a regular file", path)
	}
	return nil
}

func quarantine(path string) (string, error) {
	target := fmt.Sprintf("%s.corrupt-%s", path, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(path, target); err != nil {
		return "", fmt.Errorf("quarantine corrupt persistence file: %w", err)
	}
	return target, nil
}
