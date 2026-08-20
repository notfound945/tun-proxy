package launchservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageCopyRejectsSymlinkSource(t *testing.T) {
	directory := t.TempDir()
	real := filepath.Join(directory, "real")
	writeTestFile(t, real, "data", 0o600)
	link := filepath.Join(directory, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := stageCopy(link, filepath.Join(directory, "target"), 0o600, 1024); err == nil {
		t.Fatal("symlink source was accepted")
	}
}

func TestActivateStageRollbackRestoresOriginal(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	stage := filepath.Join(directory, "stage")
	writeTestFile(t, target, "old", 0o600)
	writeTestFile(t, stage, "new", 0o600)
	replaced, err := activateStage(stage, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, target); got != "new" {
		t.Fatalf("active contents = %q", got)
	}
	if err := replaced.rollback(); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, target); got != "old" {
		t.Fatalf("rolled back contents = %q", got)
	}
	assertNoTransactionResidue(t, directory)
}

func TestActivateStageRefusesExistingAndSymlinkTargets(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	stage := filepath.Join(directory, "stage")
	writeTestFile(t, target, "old", 0o600)
	writeTestFile(t, stage, "new", 0o600)
	if _, err := activateStage(stage, target, true); err == nil {
		t.Fatal("existing install artifact was accepted")
	} else if !strings.Contains(err.Error(), UpgradeCommand) {
		t.Fatalf("existing artifact error = %v, want complete upgrade command %q", err, UpgradeCommand)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", target); err != nil {
		t.Fatal(err)
	}
	if _, err := activateStage(stage, target, false); err == nil {
		t.Fatal("symlink target was accepted")
	}
}

func TestCommitRemovalsKeepsUninstallCommittedAfterCleanupFailure(t *testing.T) {
	directory := t.TempDir()
	targets := []string{
		filepath.Join(directory, "first"),
		filepath.Join(directory, "blocked"),
		filepath.Join(directory, "last"),
	}
	removals := make([]removal, 0, len(targets))
	for _, target := range targets {
		writeTestFile(t, target, filepath.Base(target), 0o600)
		removed, err := stageRemoval(target)
		if err != nil {
			t.Fatal(err)
		}
		removals = append(removals, removed)
	}

	// Replace the middle tombstone with a non-empty directory so its cleanup
	// fails after the first removal has already crossed the irreversible boundary.
	blockedTombstone := removals[1].tombstone
	if err := os.Remove(blockedTombstone); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blockedTombstone, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(blockedTombstone, "residue"), "preserve for diagnosis", 0o600)

	err := commitRemovals(removals)
	if err == nil || !strings.Contains(err.Error(), targets[1]) {
		t.Fatalf("commitRemovals() error = %v", err)
	}
	for _, target := range targets {
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("committed uninstall restored target %q: %v", target, err)
		}
	}
	for _, index := range []int{0, 2} {
		if _, err := os.Lstat(removals[index].tombstone); !os.IsNotExist(err) {
			t.Errorf("cleanup did not continue for tombstone %q: %v", removals[index].tombstone, err)
		}
	}
	if info, err := os.Lstat(blockedTombstone); err != nil || !info.IsDir() {
		t.Fatalf("failed cleanup residue = %v, %v", info, err)
	}
}

func TestStageRemovalRollbackAndCommit(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	writeTestFile(t, target, "preserve", 0o600)
	removed, err := stageRemoval(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target still visible after staged removal: %v", err)
	}
	if err := removed.rollback(); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, target); got != "preserve" {
		t.Fatalf("rollback contents = %q", got)
	}
	removed, err = stageRemoval(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := removed.commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target remains after commit: %v", err)
	}
	assertNoTransactionResidue(t, directory)
}

func assertNoTransactionResidue(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tun-proxy-") {
			t.Errorf("transaction residue remains: %s", entry.Name())
		}
	}
}

func writeTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
