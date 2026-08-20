//go:build darwin

package system

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

type commandCall struct {
	executable string
	args       []string
}

type fakeRunner struct {
	responses map[string]string
	calls     []commandCall
	fail      map[string]error
}

func (runner *fakeRunner) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	key := executable + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, commandCall{executable: executable, args: append([]string(nil), args...)})
	if err := runner.fail[key]; err != nil {
		return nil, err
	}
	return []byte(runner.responses[key]), nil
}

func TestSnapshotApplyAndRestoreDNS(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		networkSetup + " -getdnsservers Wi-Fi": "8.8.8.8\n8.8.4.4\n",
	}, fail: make(map[string]error)}
	states, err := snapshotDNSStates(t.Context(), runner, []networkService{{Name: "Wi-Fi", Device: "en0"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || len(states[0].Previous) != 2 {
		t.Fatalf("states = %#v", states)
	}
	applied, err := ApplyDNS(t.Context(), runner, states, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	runner.responses[networkSetup+" -getdnsservers Wi-Fi"] = "127.0.0.1\n"
	remaining, err := RestoreDNS(t.Context(), runner, applied)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("RestoreDNS() = (%v, %v)", remaining, err)
	}
	last := runner.calls[len(runner.calls)-1]
	if !reflect.DeepEqual(last.args, []string{"-setdnsservers", "Wi-Fi", "8.8.8.8", "8.8.4.4"}) {
		t.Fatalf("last restore call = %v", last.args)
	}
}

func TestParseNetworkServiceOrder(t *testing.T) {
	output := `An asterisk (*) denotes that a network service is disabled.
(1) AX88179A
(Hardware Port: AX88179A, Device: en7)

(2) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(*) Disabled
(Hardware Port: Ethernet, Device: en9)
`
	got := parseNetworkServiceOrder(output)
	want := []networkService{{Name: "AX88179A", Device: "en7"}, {Name: "Wi-Fi", Device: "en0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseNetworkServiceOrder() = %#v, want %#v", got, want)
	}
}

func TestRestoreDNSRefusesExternalChange(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		networkSetup + " -getdnsservers Wi-Fi": "9.9.9.9\n",
	}, fail: make(map[string]error)}
	state := DNSState{Service: "Wi-Fi", Previous: []string{"8.8.8.8"}, Applied: []string{"127.0.0.1"}}
	remaining, err := RestoreDNS(t.Context(), runner, []DNSState{state})
	if err == nil || len(remaining) != 1 || !strings.Contains(err.Error(), "refuse to restore") {
		t.Fatalf("RestoreDNS() = (%v, %v)", remaining, err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("unexpected mutation calls: %v", runner.calls)
	}
}

func TestApplyDNSReturnsCompletedPrefixOnFailure(t *testing.T) {
	failedKey := networkSetup + " -setdnsservers USB LAN 127.0.0.1"
	runner := &fakeRunner{responses: make(map[string]string), fail: map[string]error{failedKey: errors.New("failed")}}
	states := []DNSState{{Service: "Wi-Fi"}, {Service: "USB LAN"}}
	applied, err := ApplyDNS(t.Context(), runner, states, netip.MustParseAddr("127.0.0.1"))
	if err == nil || len(applied) != 1 || applied[0].Service != "Wi-Fi" {
		t.Fatalf("ApplyDNS() = (%v, %v)", applied, err)
	}
}
