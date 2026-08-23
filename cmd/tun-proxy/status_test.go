package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
)

func TestStatusFlagsIncludeFakeIPMappings(t *testing.T) {
	options := statusOptions{}
	flags := newStatusFlagSet(io.Discard, &options)
	if err := flags.Parse([]string{"-state", "/tmp/state.json", "-fake-ip", "-json"}); err != nil {
		t.Fatal(err)
	}
	if options.statePath != "/tmp/state.json" || !options.showFakeIP || !options.jsonOutput {
		t.Fatalf("options = %+v", options)
	}
}

func TestWriteFakeIPMappings(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 24, 1, 2, 3, 400, time.FixedZone("CST", 8*60*60))
	mappings := &runtimestatus.MappingSet{
		IPv4: []runtimestatus.Mapping{{Domain: "example.com", Address: "198.18.0.10", ExpiresAt: expiresAt}},
		IPv6: []runtimestatus.Mapping{{Domain: "ipv6.example.com", Address: "fd00:7::a", ExpiresAt: expiresAt}},
	}
	var output bytes.Buffer
	if err := writeFakeIPMappings(&output, mappings); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"fake_ip_mappings ipv4=1 ipv6=1",
		"fake_ip_mapping family=ipv4 address=198.18.0.10 domain=example.com expires=2026-08-23T17:02:03.0000004Z",
		"fake_ip_mapping family=ipv6 address=fd00:7::a domain=ipv6.example.com expires=2026-08-23T17:02:03.0000004Z",
		"",
	}, "\n")
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestWriteFakeIPMappingsRejectsMissingSection(t *testing.T) {
	if err := writeFakeIPMappings(io.Discard, nil); err == nil {
		t.Fatal("writeFakeIPMappings(nil) succeeded")
	}
}
