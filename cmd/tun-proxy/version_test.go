package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionSelectors(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := version, commit, buildTime
	version, commit, buildTime = "1.2.3", "abc123", "2026-08-20T10:00:00+08:00"
	t.Cleanup(func() {
		version, commit, buildTime = oldVersion, oldCommit, oldBuildTime
	})

	for _, selector := range []string{"version", "-version", "--version"} {
		t.Run(selector, func(t *testing.T) {
			var output bytes.Buffer
			if err := runWithVersionOutput([]string{selector}, &output); err != nil {
				t.Fatalf("runWithVersionOutput(%q) error = %v", selector, err)
			}
			want := "tun-proxy 1.2.3 (commit abc123, built 2026-08-20T10:00:00+08:00)\n"
			if output.String() != want {
				t.Fatalf("output = %q, want %q", output.String(), want)
			}
		})
	}
}

func TestVersionSelectorsRejectArguments(t *testing.T) {
	for _, selector := range []string{"version", "-version", "--version"} {
		var output bytes.Buffer
		err := runWithVersionOutput([]string{selector, "unexpected"}, &output)
		if err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
			t.Fatalf("runWithVersionOutput(%q) error = %v", selector, err)
		}
		if output.Len() != 0 {
			t.Fatalf("runWithVersionOutput(%q) output = %q, want empty", selector, output.String())
		}
	}
}
