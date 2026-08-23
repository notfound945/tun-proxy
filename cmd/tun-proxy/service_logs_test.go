package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/launchservice"
)

func TestLastLines(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		count    int
		want     string
	}{
		{name: "trailing newline", contents: "one\ntwo\nthree\nfour\n", count: 2, want: "three\nfour\n"},
		{name: "no trailing newline", contents: "one\ntwo\nthree", count: 2, want: "two\nthree"},
		{name: "single line", contents: "one", count: 1, want: "one"},
		{name: "blank trailing line", contents: "one\ntwo\n\n", count: 2, want: "two\n\n"},
		{name: "all lines", contents: "one\ntwo\n", count: 10, want: "one\ntwo\n"},
		{name: "zero", contents: "one\n", count: 0, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := string(lastLines([]byte(test.contents), test.count)); got != test.want {
				t.Fatalf("lastLines() = %q, want %q", got, test.want)
			}
		})
	}
}

type managedLogTestWriter chan string

func (writer managedLogTestWriter) Write(contents []byte) (int, error) {
	writer <- string(append([]byte(nil), contents...))
	return len(contents), nil
}

func TestServiceLogsClearFollowAcceptsMissingFile(t *testing.T) {
	directory := t.TempDir()
	layout := launchservice.Layout{StandardOut: filepath.Join(directory, "stdout.log")}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := serviceLogsCommand(ctx, layout, []string{"-clear", "-follow", "-stream", "stdout"}); err != nil {
		t.Fatalf("serviceLogsCommand() error = %v", err)
	}
}

func TestFollowManagedLogsWaitsForMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writes := make(managedLogTestWriter, 1)
	errors := make(chan error, 1)
	go func() {
		errors <- followManagedLogsAtInterval(ctx, []followedLog{{managedLog: managedLog{Name: "stdout", Path: path}}}, writes, 5*time.Millisecond)
	}()

	select {
	case err := <-errors:
		t.Fatalf("followManagedLogsAtInterval() exited while log was absent: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	if err := os.WriteFile(path, []byte("created after follow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-writes:
		if got != "created after follow\n" {
			t.Fatalf("followed contents = %q", got)
		}
	case err := <-errors:
		t.Fatalf("followManagedLogsAtInterval() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("follow mode did not read a log created after startup")
	}

	cancel()
	select {
	case err := <-errors:
		if err != nil {
			t.Fatalf("followManagedLogsAtInterval() cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follow mode did not stop after cancellation")
	}
}

func TestReadManagedLogRangeStopsAtCapturedSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	initial := []byte("before snapshot\n")
	appended := []byte("after snapshot\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	file, info, err := openManagedLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // Test cleanup.
	appendFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFile.Write(appended); err != nil {
		_ = appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := readManagedLogRange(file, path, 0, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(initial) {
		t.Fatalf("snapshot contents = %q, want %q", contents, initial)
	}
	current, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	contents, err = readManagedLogRange(file, path, info.Size(), current.Size())
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(appended) {
		t.Fatalf("next snapshot contents = %q, want %q", contents, appended)
	}
}

func TestTailManagedLogRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	real := filepath.Join(directory, "stdout.log")
	if err := os.WriteFile(real, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.log")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tailManagedLog(link, 10); err == nil {
		t.Fatal("tailManagedLog accepted a symlink")
	}
}

func TestServiceInstallLogDiagnosticsIncludesOnlyCurrentAttempt(t *testing.T) {
	directory := t.TempDir()
	layout := launchservice.Layout{
		StandardOut: filepath.Join(directory, "stdout.log"),
		StandardErr: filepath.Join(directory, "stderr.log"),
	}
	if err := os.WriteFile(layout.StandardOut, []byte("old stdout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.StandardErr, []byte("old stderr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoints := checkpointManagedLogs(layout)
	stdout, err := os.OpenFile(layout.StandardOut, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.WriteString("current stdout\n"); err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(layout.StandardErr, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.WriteString("current stderr\n"); err != nil {
		_ = stderr.Close()
		t.Fatal(err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}

	want := errors.New("service did not become ready")
	got := withServiceInstallLogDiagnostics(want, layout, checkpoints)
	if !errors.Is(got, want) {
		t.Fatalf("withServiceInstallLogDiagnostics() error = %v, want wrapped %v", got, want)
	}
	for _, text := range []string{"service output from this install attempt", "current stderr", "current stdout"} {
		if !strings.Contains(got.Error(), text) {
			t.Fatalf("diagnostic error = %q, want %q", got, text)
		}
	}
	for _, stale := range []string{"old stderr", "old stdout"} {
		if strings.Contains(got.Error(), stale) {
			t.Fatalf("diagnostic error included stale output %q: %q", stale, got)
		}
	}
}

func TestClearManagedLogsTruncatesSelectedFiles(t *testing.T) {
	directory := t.TempDir()
	stdoutPath := filepath.Join(directory, "stdout.log")
	stderrPath := filepath.Join(directory, "stderr.log")
	if err := os.WriteFile(stdoutPath, []byte("stdout contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte("stderr contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleared, err := clearManagedLogs([]managedLog{{Name: "stderr", Path: stderrPath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 1 || cleared[0].Name != "stderr" {
		t.Fatalf("clearManagedLogs() cleared = %+v, want stderr", cleared)
	}
	stderrContents, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(stderrContents) != 0 {
		t.Fatalf("stderr after clear = %q, want empty", stderrContents)
	}
	stdoutContents, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdoutContents) != "stdout contents\n" {
		t.Fatalf("unselected stdout after clear = %q", stdoutContents)
	}
}

func TestClearManagedLogsPreflightsAllPathsBeforeTruncating(t *testing.T) {
	directory := t.TempDir()
	regularPath := filepath.Join(directory, "stdout.log")
	targetPath := filepath.Join(directory, "target.log")
	symlinkPath := filepath.Join(directory, "stderr.log")
	if err := os.WriteFile(regularPath, []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Fatal(err)
	}

	_, err := clearManagedLogs([]managedLog{
		{Name: "stdout", Path: regularPath},
		{Name: "stderr", Path: symlinkPath},
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("clearManagedLogs() error = %v, want unsafe path rejection", err)
	}
	contents, readErr := os.ReadFile(regularPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(contents) != "preserve me\n" {
		t.Fatalf("preflight failure modified earlier log: %q", contents)
	}
}

func TestClearManagedLogsTreatsMissingFilesAsAlreadyClear(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.log")
	cleared, err := clearManagedLogs([]managedLog{{Name: "stderr", Path: missing}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Fatalf("clearManagedLogs() cleared = %+v, want none", cleared)
	}
}
