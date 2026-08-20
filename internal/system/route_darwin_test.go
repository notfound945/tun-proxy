//go:build darwin

package system

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestParseRouteLookup(t *testing.T) {
	output := `   route to: 198.18.0.1
destination: default
       mask: default
    gateway: 192.0.2.1
  interface: en0
`
	lookup, err := ParseRouteLookup(output)
	if err != nil {
		t.Fatal(err)
	}
	if lookup.Destination != "default" || lookup.Mask != "default" || lookup.Interface != "en0" || lookup.Gateway != "192.0.2.1" {
		t.Fatalf("lookup = %+v", lookup)
	}
}

func TestSplitDefaultRouteIsNotOrdinaryDefault(t *testing.T) {
	if defaultRouteLookup(RouteLookup{Destination: "default", Mask: "128.0.0.0"}) {
		t.Fatal("IPv4 /1 route was recognized as the ordinary default")
	}
	if defaultRouteLookup(RouteLookup{Destination: "::", Mask: "8000::"}) {
		t.Fatal("IPv6 /1 route was recognized as the ordinary default")
	}
	if !defaultRouteLookup(RouteLookup{Destination: "default", Mask: "default"}) {
		t.Fatal("ordinary default route was not recognized")
	}
}

func TestIPv4SplitDefaultRouteLifecycle(t *testing.T) {
	route := RouteState{Prefix: "0.0.0.0/1", Interface: "utun4"}
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get 0.0.0.1": "destination: default\nmask: 128.0.0.0\ninterface: utun4\n",
	}, fail: make(map[string]error)}
	if err := AddRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-n add -net 0.0.0.0/1 -interface utun4",
		"-n get 0.0.0.1",
		"-n get 0.0.0.1",
		"-n delete -net 0.0.0.0/1 -interface utun4",
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	for index, call := range runner.calls {
		if got := strings.Join(call.args, " "); got != want[index] {
			t.Fatalf("call %d = %q, want %q", index, got, want[index])
		}
	}
}

func TestScopedRouteLookupArgumentsSelectInterfaceAndFamily(t *testing.T) {
	got := routeLookupScopedArgs(netip.MustParseAddr("2001:db8::53"), "en7")
	want := []string{"-n", "get", "-inet6", "-ifscope", "en7", "2001:db8::53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routeLookupScopedArgs() = %v, want %v", got, want)
	}
}

func TestScopedDefaultRouteLookupArguments(t *testing.T) {
	got := routeDefaultLookupScopedArgs(true, "en7")
	want := []string{"-n", "get", "-inet6", "-ifscope", "en7", "default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routeDefaultLookupScopedArgs() = %v, want %v", got, want)
	}
}

func TestDefaultRouteLookupArguments(t *testing.T) {
	got := routeDefaultLookupArgs(true)
	want := []string{"-n", "get", "-inet6", "default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routeDefaultLookupArgs() = %v, want %v", got, want)
	}
}

func TestGatewayHostRouteLifecycle(t *testing.T) {
	route := RouteState{Prefix: "1.1.1.1/32", Interface: "en0", Gateway: "192.0.2.1"}
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get 1.1.1.1": "destination: 1.1.1.1\ngateway: 192.0.2.1\ninterface: en0\n",
	}, fail: make(map[string]error)}
	if err := AddRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-n add -host 1.1.1.1 192.0.2.1",
		"-n get 1.1.1.1",
		"-n get 1.1.1.1",
		"-n delete -host 1.1.1.1 192.0.2.1",
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	for index, call := range runner.calls {
		if got := strings.Join(call.args, " "); got != want[index] {
			t.Fatalf("call %d = %q, want %q", index, got, want[index])
		}
	}
}

func TestScopedIPv4SplitRouteLifecycle(t *testing.T) {
	route := RouteState{
		Prefix: "0.0.0.0/1", Interface: "en7", Gateway: "192.0.2.1", Scope: "en7",
	}
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get -ifscope en7 0.0.0.1": "destination: default\nmask: 128.0.0.0\ngateway: 192.0.2.1\ninterface: en7\n",
	}, fail: make(map[string]error)}
	if err := AddRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-n add -net -ifscope en7 0.0.0.0/1 192.0.2.1",
		"-n get -ifscope en7 0.0.0.1",
		"-n get -ifscope en7 0.0.0.1",
		"-n delete -net -ifscope en7 0.0.0.0/1 192.0.2.1",
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	for index, call := range runner.calls {
		if got := strings.Join(call.args, " "); got != want[index] {
			t.Fatalf("call %d = %q, want %q", index, got, want[index])
		}
	}
}

func TestScopedIPv6SplitRouteLifecycle(t *testing.T) {
	route := RouteState{
		Prefix: "::/1", Interface: "en7", Gateway: "fe80::1%en7", Scope: "en7",
	}
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get -inet6 -ifscope en7 ::2": "destination: ::\nmask: 8000::\ngateway: fe80::1%en7\ninterface: en7\n",
	}, fail: make(map[string]error)}
	if err := AddRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-n add -inet6 -net -ifscope en7 ::/1 fe80::1%en7",
		"-n get -inet6 -ifscope en7 ::2",
		"-n get -inet6 -ifscope en7 ::2",
		"-n delete -inet6 -net -ifscope en7 ::/1 fe80::1%en7",
	}
	for index, call := range runner.calls {
		if got := strings.Join(call.args, " "); got != want[index] {
			t.Fatalf("call %d = %q, want %q", index, got, want[index])
		}
	}
}

func TestScopedSplitRouteRejectsWrongMaskAndScope(t *testing.T) {
	route := RouteState{
		Prefix: "0.0.0.0/1", Interface: "en7", Gateway: "192.0.2.1", Scope: "en7",
	}
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get -ifscope en7 0.0.0.1": "destination: default\nmask: 192.0.0.0\ngateway: 192.0.2.1\ninterface: en7\n",
	}, fail: make(map[string]error)}
	if err := VerifyRoute(t.Context(), runner, route); err == nil || !strings.Contains(err.Error(), "want /1") {
		t.Fatalf("VerifyRoute() = %v", err)
	}
	route.Scope = "en0"
	if err := AddRoute(t.Context(), runner, route); err == nil || !strings.Contains(err.Error(), "invalid route scope") {
		t.Fatalf("AddRoute() = %v", err)
	}
	route.Scope = "en7"
	route.Gateway = ""
	if err := AddRoute(t.Context(), runner, route); err == nil || !strings.Contains(err.Error(), "requires a gateway") {
		t.Fatalf("AddRoute() = %v", err)
	}
}

func TestRemoveGatewayRouteRefusesChangedGateway(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get 1.1.1.1": "destination: 1.1.1.1\ngateway: 192.0.2.254\ninterface: en0\n",
	}, fail: make(map[string]error)}
	err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "1.1.1.1/32", Interface: "en0", Gateway: "192.0.2.1"})
	if err == nil || !strings.Contains(err.Error(), "current gateway") || len(runner.calls) != 1 {
		t.Fatalf("RemoveRoute() = %v, calls=%v", err, runner.calls)
	}
}

func TestDefaultRouteDestinationSupportsBothFamilies(t *testing.T) {
	for _, destination := range []string{"default", "0.0.0.0", "::"} {
		if !defaultRouteDestination(destination) {
			t.Fatalf("%q was not recognized as a default route", destination)
		}
	}
	if defaultRouteDestination("fd00:7::/96") {
		t.Fatal("specific IPv6 prefix was recognized as a default route")
	}
}

func TestRouteLookupArgumentsSelectAddressFamily(t *testing.T) {
	tests := []struct {
		address string
		want    []string
	}{
		{address: "198.18.0.1", want: []string{"-n", "get", "198.18.0.1"}},
		{address: "fd00:7::1", want: []string{"-n", "get", "-inet6", "fd00:7::1"}},
	}
	for _, test := range tests {
		address := netip.MustParseAddr(test.address)
		if got := routeLookupArgs(address); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("routeLookupArgs(%s) = %v, want %v", address, got, test.want)
		}
	}
}

func TestCheckPrefixAvailableAcceptsMissingIPv6RoutingTable(t *testing.T) {
	call := "/sbin/route -n get -inet6 fd00:7::1"
	runner := &fakeRunner{
		responses: make(map[string]string),
		fail:      map[string]error{call: errors.New("exit status 68: route: writing to routing socket: not in table")},
	}
	if err := checkPrefixAvailable(t.Context(), runner, netip.MustParsePrefix("fd00:7::/96")); err != nil {
		t.Fatalf("checkPrefixAvailable() = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].executable != "/sbin/route" || strings.Join(runner.calls[0].args, " ") != "-n get -inet6 fd00:7::1" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestCheckPrefixAvailableAcceptsSuccessfulNotInTableOutput(t *testing.T) {
	call := "/sbin/route -n get -inet6 fd00:7::1"
	runner := &fakeRunner{
		responses: map[string]string{call: "route: writing to routing socket: not in table\n"},
		fail:      make(map[string]error),
	}
	if err := checkPrefixAvailable(t.Context(), runner, netip.MustParsePrefix("fd00:7::/96")); err != nil {
		t.Fatalf("checkPrefixAvailable() = %v", err)
	}
}

func TestCheckPrefixAvailableKeepsUnexpectedLookupFailure(t *testing.T) {
	call := "/sbin/route -n get -inet6 fd00:7::1"
	runner := &fakeRunner{
		responses: make(map[string]string),
		fail:      map[string]error{call: errors.New("permission denied")},
	}
	err := checkPrefixAvailable(t.Context(), runner, netip.MustParsePrefix("fd00:7::/96"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("checkPrefixAvailable() = %v", err)
	}
}

func TestCommandAllowlist(t *testing.T) {
	if _, err := RunCommand(t.Context(), "/bin/sh", "-c", "true"); err == nil {
		t.Fatal("RunCommand accepted /bin/sh")
	}
}

func TestRemoveRouteRefusesChangedInterface(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get 198.18.0.1": "destination: 198.18/15\ninterface: en0\n",
	}, fail: make(map[string]error)}
	err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "198.18.0.0/15", Interface: "utun7"})
	if err == nil || !strings.Contains(err.Error(), "refuse to remove") {
		t.Fatalf("RemoveRoute() = %v", err)
	}
	if len(runner.calls) != 2 || runner.calls[1].executable != "/sbin/ifconfig" || strings.Join(runner.calls[1].args, " ") != "utun7" {
		t.Fatalf("RemoveRoute issued a mutation: %v", runner.calls)
	}
}

func TestRemoveRouteTreatsMissingRecordedInterfaceAsAbsent(t *testing.T) {
	getRoute := "/sbin/route -n get -inet6 8000::1"
	getInterface := "/sbin/ifconfig utun4"
	runner := &fakeRunner{
		responses: map[string]string{
			getRoute: "destination: ::\nmask: 8000::\ngateway: fe80::1%en7\ninterface: en7\n",
		},
		fail: map[string]error{
			getInterface: errors.New("ifconfig: interface utun4 does not exist"),
		},
	}
	if err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "8000::/1", Interface: "utun4"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[1].executable != "/sbin/ifconfig" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestRemoveRouteKeepsUnexpectedInterfaceInspectionFailure(t *testing.T) {
	getRoute := "/sbin/route -n get 0.0.0.1"
	getInterface := "/sbin/ifconfig utun4"
	runner := &fakeRunner{
		responses: map[string]string{
			getRoute: "destination: default\nmask: 128.0.0.0\ngateway: 192.0.2.1\ninterface: en7\n",
		},
		fail: map[string]error{getInterface: errors.New("permission denied")},
	}
	err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "0.0.0.0/1", Interface: "utun4"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("RemoveRoute() = %v", err)
	}
}

func TestRemoveRouteDeletesRecordedRoute(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get 198.18.0.1": "destination: 198.18/15\ninterface: utun7\n",
	}, fail: make(map[string]error)}
	if err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "198.18.0.0/15", Interface: "utun7"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[1].args, " "); got != "-n delete -net 198.18.0.0/15 -interface utun7" {
		t.Fatalf("delete args = %q", got)
	}
}

func TestVerifyRouteRequiresRecordedInterface(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get 198.18.0.1": "destination: 198.18/15\ninterface: utun7\n",
	}, fail: make(map[string]error)}
	route := RouteState{Prefix: "198.18.0.0/15", Interface: "utun7"}
	if err := VerifyRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	runner.responses["/sbin/route -n get 198.18.0.1"] = "destination: default\ninterface: en0\n"
	if err := VerifyRoute(t.Context(), runner, route); err == nil {
		t.Fatal("VerifyRoute accepted the default route")
	}
}

func TestIPv6RouteUsesExplicitAddressFamily(t *testing.T) {
	route := RouteState{Prefix: "fd00:7::/96", Interface: "utun7"}
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get -inet6 fd00:7::1": "destination: fd00:7::/96\ninterface: utun7\n",
	}, fail: make(map[string]error)}
	if err := AddRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(t.Context(), runner, route); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-n add -inet6 -net fd00:7::/96 -interface utun7",
		"-n get -inet6 fd00:7::1",
		"-n get -inet6 fd00:7::1",
		"-n delete -inet6 -net fd00:7::/96 -interface utun7",
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	for index, call := range runner.calls {
		if got := strings.Join(call.args, " "); got != want[index] {
			t.Fatalf("call %d args = %q, want %q", index, got, want[index])
		}
	}
}

func TestRemoveIPv6RouteTreatsDefaultAsAbsent(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get -inet6 fd00:7::1": "destination: ::\ninterface: en0\n",
	}, fail: make(map[string]error)}
	if err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "fd00:7::/96", Interface: "utun7"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("absent route issued mutation: %#v", runner.calls)
	}
}

func TestRemoveIPv6RouteTreatsNotInTableAsAbsent(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/sbin/route -n get -inet6 fd00:7::1": "route: writing to routing socket: not in table\n",
	}, fail: make(map[string]error)}
	if err := RemoveRoute(t.Context(), runner, RouteState{Prefix: "fd00:7::/96", Interface: "utun7"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("absent route issued mutation: %#v", runner.calls)
	}
}
