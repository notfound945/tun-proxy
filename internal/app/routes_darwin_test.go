//go:build darwin

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/system"
)

type routeTestRunner struct {
	calls     []string
	responses map[string]string
	fail      map[string]error
}

func (runner *routeTestRunner) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	call := executable + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	if err := runner.fail[call]; err != nil {
		return nil, err
	}
	return []byte(runner.responses[call]), nil
}

func TestRemoveRecordedRoutesUsesReverseOrderAndPersistsProgress(t *testing.T) {
	path, state := writeRouteTestState(t)
	runner := &routeTestRunner{responses: map[string]string{
		"/sbin/route -n get -inet6 fd00:7::1": "destination: fd00:7::/96\ninterface: utun7\n",
		"/sbin/route -n get 198.18.0.1":       "destination: 198.18/15\ninterface: utun7\n",
	}, fail: make(map[string]error)}
	if err := removeRecordedRoutes(t.Context(), runner, path, &state); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/sbin/route -n get -inet6 fd00:7::1",
		"/sbin/route -n delete -inet6 -net fd00:7::/96 -interface utun7",
		"/sbin/route -n get 198.18.0.1",
		"/sbin/route -n delete -net 198.18.0.0/15 -interface utun7",
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}
	persisted, err := system.ReadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Route != nil || len(persisted.Routes) != 0 {
		t.Fatalf("persisted routes = legacy:%+v additional:%+v", persisted.Route, persisted.Routes)
	}
}

func TestRemoveRecordedRoutesStopsBeforeIPv4WhenIPv6Fails(t *testing.T) {
	path, state := writeRouteTestState(t)
	getIPv6 := "/sbin/route -n get -inet6 fd00:7::1"
	runner := &routeTestRunner{responses: make(map[string]string), fail: map[string]error{getIPv6: errors.New("lookup failed")}}
	err := removeRecordedRoutes(t.Context(), runner, path, &state)
	if err == nil || !strings.Contains(err.Error(), "fd00:7::/96") {
		t.Fatalf("removeRecordedRoutes() = %v", err)
	}
	if !reflect.DeepEqual(runner.calls, []string{getIPv6}) || state.Route == nil || len(state.Routes) != 1 {
		t.Fatalf("calls=%v state=%+v", runner.calls, state)
	}
}

func TestRemoveRecordedRoutesDeletesGlobalBeforeScopedSplitRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := system.NewState("sha256:test")
	state.Routes = []system.RouteState{
		{Prefix: "0.0.0.0/1", Interface: "en7", Gateway: "192.0.2.1", Scope: "en7"},
		{Prefix: "0.0.0.0/1", Interface: "utun4"},
	}
	if err := system.WriteState(path, state); err != nil {
		t.Fatal(err)
	}
	runner := &routeTestRunner{responses: map[string]string{
		"/sbin/route -n get 0.0.0.1":              "destination: default\nmask: 128.0.0.0\ninterface: utun4\n",
		"/sbin/route -n get -ifscope en7 0.0.0.1": "destination: default\nmask: 128.0.0.0\ngateway: 192.0.2.1\ninterface: en7\n",
	}, fail: make(map[string]error)}
	if err := removeRecordedRoutes(t.Context(), runner, path, &state); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/sbin/route -n get 0.0.0.1",
		"/sbin/route -n delete -net 0.0.0.0/1 -interface utun4",
		"/sbin/route -n get -ifscope en7 0.0.0.1",
		"/sbin/route -n delete -net -ifscope en7 0.0.0.0/1 192.0.2.1",
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}
}

func writeRouteTestState(t *testing.T) (string, system.State) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	state := system.NewState("sha256:test")
	state.Route = &system.RouteState{Prefix: "198.18.0.0/15", Interface: "utun7"}
	state.Routes = []system.RouteState{{Prefix: "fd00:7::/96", Interface: "utun7"}}
	if err := system.WriteState(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path, state
}
