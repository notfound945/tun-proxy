package main

import (
	"strings"
	"testing"
)

func TestCleanupCommandRejectsNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s"} {
		err := cleanupCommand([]string{"-timeout", timeout})
		if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
			t.Fatalf("cleanupCommand(-timeout %s) error = %v", timeout, err)
		}
	}
}
