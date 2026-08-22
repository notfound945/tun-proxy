package main

import (
	"bytes"
	"flag"
	"io"
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
	if usage := renderedUsage("config"); !strings.Contains(usage, "-finder") {
		t.Fatalf("config help missing Finder flag: %s", usage)
	}
	if !strings.Contains(output.String(), "-version") {
		t.Fatalf("top-level help missing version flag:\n%s", output.String())
	}
	if usage := renderedUsage("cleanup"); !strings.Contains(usage, "-timeout DURATION") {
		t.Fatalf("cleanup help missing timeout flag: %s", usage)
	}
	if usage := renderedUsage("cleanup"); !strings.Contains(usage, "-clear-dns") {
		t.Fatalf("cleanup help missing DNS clear flag: %s", usage)
	}
	if usage := renderedUsage("service install"); !strings.Contains(usage, "-start-at-boot") {
		t.Fatalf("service install help missing boot-start flag: %s", usage)
	}
	for _, topic := range []string{"service start", "service stop", "service reload", "service upgrade"} {
		if usage := renderedUsage(topic); !strings.Contains(usage, serviceLogsHintCommand) {
			t.Fatalf("%s help missing logs hint: %s", topic, usage)
		}
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
		usage := renderedUsage(topic)
		if !strings.Contains(usage, "usage: tun-proxy "+topic) {
			t.Fatalf("help topic %q is missing or malformed: %q", topic, usage)
		}
	}
}

func TestGeneratedHelpContainsEveryCommandFlag(t *testing.T) {
	for topic, template := range commandUsages {
		flags, hasFlags := commandFlagSet(topic, io.Discard)
		hasMarker := strings.Contains(template, flagOptionsMarker)
		if hasFlags != hasMarker {
			t.Fatalf("help topic %q flag set=%t marker=%t", topic, hasFlags, hasMarker)
		}
		if !hasFlags {
			continue
		}
		usage := renderedUsage(topic)
		if strings.Contains(usage, flagOptionsMarker) {
			t.Fatalf("help topic %q retained generated-options marker", topic)
		}
		flags.VisitAll(func(item *flag.Flag) {
			if !usageContainsFlag(usage, item.Name) {
				t.Errorf("help topic %q missing registered flag -%s", topic, item.Name)
			}
		})
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

func renderedUsage(topic string) string {
	var output bytes.Buffer
	fprintUsage(&output, strings.Fields(topic))
	return output.String()
}

func usageContainsFlag(usage, name string) bool {
	for _, line := range strings.Split(usage, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && fields[0] == "-"+name {
			return true
		}
	}
	return false
}
