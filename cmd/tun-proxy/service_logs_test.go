package main

import (
	"os"
	"path/filepath"
	"testing"
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
