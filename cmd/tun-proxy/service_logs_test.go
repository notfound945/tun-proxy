package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
