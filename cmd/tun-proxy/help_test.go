package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpContainsOperationalCommands(t *testing.T) {
	var output bytes.Buffer
	fprintUsage(&output, nil)
	for _, command := range []string{
		"config [options|command]", "explain", "diagnose", "service <command>",
	} {
		if !strings.Contains(output.String(), command) {
			t.Fatalf("top-level help missing %q:\n%s", command, output.String())
		}
	}
	if usage := commandUsages["config"]; !strings.Contains(usage, "-finder") {
		t.Fatalf("config help missing Finder flag: %s", usage)
	}
	if !strings.Contains(output.String(), "-version") {
		t.Fatalf("top-level help missing version flag:\n%s", output.String())
	}

	for _, topic := range []string{
		"config validate",
		"explain",
		"diagnose",
		"service stop",
		"service restart",
		"service reload",
		"service logs",
	} {
		usage, ok := commandUsages[topic]
		if !ok || !strings.Contains(usage, "usage: tun-proxy "+topic) {
			t.Fatalf("help topic %q is missing or malformed: %q", topic, usage)
		}
	}
}

func TestHelpRejectsUnknownTopic(t *testing.T) {
	if err := helpCommand([]string{"not-a-command"}); err == nil || !strings.Contains(err.Error(), "unknown help topic") {
		t.Fatalf("helpCommand() error = %v", err)
	}
}

func TestServiceRestartHelpAlias(t *testing.T) {
	if err := serviceCommand([]string{"restart", "-h"}); err != nil {
		t.Fatalf("service restart -h error = %v", err)
	}
}
