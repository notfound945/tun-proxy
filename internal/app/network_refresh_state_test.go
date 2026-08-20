package app

import (
	"errors"
	"testing"
)

func TestNetworkRefreshRetriesWhenTopologyRecoversToOriginalFingerprint(t *testing.T) {
	state := newNetworkRefreshState("en0=up,en7=up")
	if state.shouldAttempt("en0=up,en7=up", false) {
		t.Fatal("unchanged healthy topology requested a refresh")
	}
	if !state.shouldAttempt("en0=up,en7=down", false) {
		t.Fatal("topology failure did not request a refresh")
	}
	if !state.failed(errors.New("en7 has no IPv4")) {
		t.Fatal("first failure was not loggable")
	}
	if !state.shouldAttempt("en0=up,en7=up", false) {
		t.Fatal("recovery to original fingerprint skipped the pending refresh")
	}
	state.succeeded("en0=up,en7=up")
	if state.shouldAttempt("en0=up,en7=up", false) {
		t.Fatal("successful recovery remained pending")
	}
}

func TestNetworkRefreshDeduplicatesRepeatedFailureLogs(t *testing.T) {
	state := newNetworkRefreshState("healthy")
	err := errors.New("interface unavailable")
	if !state.failed(err) {
		t.Fatal("first failure was not loggable")
	}
	if state.failed(err) {
		t.Fatal("unchanged failure was loggable twice")
	}
	if !state.failed(errors.New("interface has no IPv4")) {
		t.Fatal("changed failure was not loggable")
	}
}
