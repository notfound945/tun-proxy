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
	states, err := snapshotDNSStates(t.Context(), runner, []networkService{{Name: "Wi-Fi", Device: "en0"}}, netip.MustParseAddr("127.0.0.1"))
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

func TestSnapshotDNSRefusesReplacementAsPrevious(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		networkSetup + " -getdnsservers Wi-Fi": "8.8.8.8\n127.0.0.1\n",
	}, fail: make(map[string]error)}
	states, err := snapshotDNSStates(
		t.Context(), runner, []networkService{{Name: "Wi-Fi", Device: "en0"}}, netip.MustParseAddr("127.0.0.1"),
	)
	if err == nil || !strings.Contains(err.Error(), "replacement address") {
		t.Fatalf("snapshotDNSStates() = (%v, %v)", states, err)
	}
	if states != nil {
		t.Fatalf("snapshotDNSStates() returned partial snapshot: %v", states)
	}
}

func TestPlanDNSRefusesReplacementAlreadyRecordedAsPrevious(t *testing.T) {
	states := []DNSState{{Service: "Wi-Fi", Previous: []string{"8.8.8.8", "127.0.0.1"}}}
	planned, err := PlanDNS(states, netip.MustParseAddr("127.0.0.1"))
	if err == nil || !strings.Contains(err.Error(), "replacement address") {
		t.Fatalf("PlanDNS() = (%v, %v)", planned, err)
	}
	if planned != nil {
		t.Fatalf("PlanDNS() returned partial plan: %v", planned)
	}
}

func TestPlanDNSPreservesDifferentLoopbackResolver(t *testing.T) {
	states := []DNSState{{Service: "Wi-Fi", Previous: []string{"127.0.0.2"}}}
	planned, err := PlanDNS(states, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := planned[0].Previous; !reflect.DeepEqual(got, []string{"127.0.0.2"}) {
		t.Fatalf("Previous = %v", got)
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

func TestClearDNSReplacementOnlyResetsExactManagedValue(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		networkSetup + " -listnetworkserviceorder": `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) USB LAN
(Hardware Port: USB LAN, Device: en7)

(3) Custom DNS
(Hardware Port: Ethernet, Device: en8)

(*) Disabled
(Hardware Port: Ethernet, Device: en9)
`,
		networkSetup + " -getdnsservers Wi-Fi":      "127.0.0.1\n",
		networkSetup + " -getdnsservers USB LAN":    "9.9.9.9\n",
		networkSetup + " -getdnsservers Custom DNS": "127.0.0.1\n8.8.8.8\n",
	}, fail: make(map[string]error)}

	cleared, err := ClearDNSReplacement(t.Context(), runner, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cleared, []string{"Wi-Fi"}) {
		t.Fatalf("cleared = %v", cleared)
	}
	wantMutation := []string{"-setdnsservers", "Wi-Fi", "Empty"}
	mutations := 0
	for _, call := range runner.calls {
		if len(call.args) != 0 && call.args[0] == "-setdnsservers" {
			mutations++
			if !reflect.DeepEqual(call.args, wantMutation) {
				t.Fatalf("mutation = %v, want %v", call.args, wantMutation)
			}
		}
	}
	if mutations != 1 {
		t.Fatalf("mutation count = %d, calls = %v", mutations, runner.calls)
	}
}

func TestClearDNSReplacementReportsInspectionFailuresAndContinues(t *testing.T) {
	failedKey := networkSetup + " -getdnsservers Wi-Fi"
	runner := &fakeRunner{responses: map[string]string{
		networkSetup + " -listnetworkserviceorder": `(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) USB LAN
(Hardware Port: USB LAN, Device: en7)
`,
		networkSetup + " -getdnsservers USB LAN": "127.0.0.1\n",
	}, fail: map[string]error{failedKey: errors.New("inspection failed")}}

	cleared, err := ClearDNSReplacement(t.Context(), runner, netip.MustParseAddr("127.0.0.1"))
	if err == nil || !strings.Contains(err.Error(), "inspection failed") {
		t.Fatalf("ClearDNSReplacement() error = %v", err)
	}
	if !reflect.DeepEqual(cleared, []string{"USB LAN"}) {
		t.Fatalf("cleared = %v", cleared)
	}
}

func TestClearDNSReplacementRejectsNonLoopback(t *testing.T) {
	if _, err := ClearDNSReplacement(t.Context(), &fakeRunner{}, netip.MustParseAddr("192.0.2.1")); err == nil {
		t.Fatal("ClearDNSReplacement accepted a non-loopback replacement")
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
