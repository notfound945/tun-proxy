//go:build darwin

package app

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/system"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
)

func TestPhase2ShutdownRemovesRouteBeforeClosingTUN(t *testing.T) {
	recorder := newEventRecorder()
	dependencies := fakePhase2Dependencies(recorder)
	runtime := phase2TestConfig()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := runPhase2(ctx, runtime, "sha256:test", nil, dependencies); err != nil {
		t.Fatal(err)
	}
	events := recorder.snapshot()
	assertEventOrder(t, events, "write-state:starting:route=true", "add-route")
	assertEventOrder(t, events, "remove-route", "close-device")
	assertEventOrder(t, events, "close-device", "pump-stop")
	assertEventOrder(t, events, "pump-stop", "close-lock")
	assertEventOrder(t, events, "close-lock", "remove-state")
}

func TestPhase2AddRouteFailureRollsBackAndRemovesState(t *testing.T) {
	recorder := newEventRecorder()
	dependencies := fakePhase2Dependencies(recorder)
	dependencies.addRoute = func(context.Context, system.RouteState) error {
		recorder.add("add-route")
		return errors.New("route failed")
	}

	_, err := runPhase2(t.Context(), phase2TestConfig(), "sha256:test", nil, dependencies)
	if err == nil || !strings.Contains(err.Error(), "route failed") {
		t.Fatalf("runPhase2() = %v", err)
	}
	events := recorder.snapshot()
	assertEventOrder(t, events, "add-route", "remove-route")
	assertEventOrder(t, events, "remove-route", "close-device")
	if !slices.Contains(events, "remove-state") {
		t.Fatalf("successful rollback retained state: %v", events)
	}
}

func TestPhase2CleanupFailureRetainsState(t *testing.T) {
	recorder := newEventRecorder()
	dependencies := fakePhase2Dependencies(recorder)
	dependencies.removeRoute = func(context.Context, system.RouteState) error {
		recorder.add("remove-route")
		return errors.New("route still busy")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := runPhase2(ctx, phase2TestConfig(), "sha256:test", nil, dependencies)
	if err == nil || !strings.Contains(err.Error(), "route still busy") {
		t.Fatalf("runPhase2() = %v", err)
	}
	if slices.Contains(recorder.snapshot(), "remove-state") {
		t.Fatalf("cleanup failure removed recovery state: %v", recorder.snapshot())
	}
}

func phase2TestConfig() *config.Config {
	return &config.Config{
		System: config.System{StateFile: "/tmp/tun-proxy-state.json", LockFile: "/tmp/tun-proxy.lock"},
		TUN:    config.TUN{Address: netip.MustParseAddr("10.255.0.2"), Peer: netip.MustParseAddr("10.255.0.1"), MTU: 1400},
		FakeIP: config.FakeIP{Prefix: netip.MustParsePrefix("198.18.0.0/15")},
	}
}

func fakePhase2Dependencies(recorder *eventRecorder) phase2Dependencies {
	return phase2Dependencies{
		acquireLock: func(string) (lifecycleLock, error) {
			recorder.add("acquire-lock")
			return fakeLifecycleLock{recorder: recorder}, nil
		},
		writeState: func(_ string, state system.State) error {
			recorder.add("write-state:" + state.Phase + ":route=" + map[bool]string{true: "true", false: "false"}[state.Route != nil])
			return nil
		},
		removeState: func(string) error {
			recorder.add("remove-state")
			return nil
		},
		createTUN: func(context.Context, config.TUN) (lifecycleDevice, error) {
			recorder.add("create-device")
			return &fakeLifecycleDevice{recorder: recorder}, nil
		},
		addRoute: func(context.Context, system.RouteState) error {
			recorder.add("add-route")
			return nil
		},
		verifyRoute: func(context.Context, system.RouteState) error {
			recorder.add("verify-route")
			return nil
		},
		removeRoute: func(context.Context, system.RouteState) error {
			recorder.add("remove-route")
			return nil
		},
		newPump: func(lifecycleDevice, int) (lifecyclePump, error) {
			recorder.add("new-pump")
			return fakeLifecyclePump{recorder: recorder}, nil
		},
	}
}

type fakeLifecycleLock struct{ recorder *eventRecorder }

func (lock fakeLifecycleLock) Close() error {
	lock.recorder.add("close-lock")
	return nil
}

type fakeLifecycleDevice struct{ recorder *eventRecorder }

func (device *fakeLifecycleDevice) Name() string { return "utun7" }
func (device *fakeLifecycleDevice) Close() error {
	device.recorder.add("close-device")
	return nil
}

type fakeLifecyclePump struct{ recorder *eventRecorder }

func (pump fakeLifecyclePump) Run(ctx context.Context) error {
	pump.recorder.add("pump-start")
	<-ctx.Done()
	pump.recorder.add("pump-stop")
	return nil
}
func (fakeLifecyclePump) Stats() internaltun.Stats { return internaltun.Stats{} }

type eventRecorder struct {
	mutex  sync.Mutex
	events []string
}

func newEventRecorder() *eventRecorder { return new(eventRecorder) }
func (recorder *eventRecorder) add(event string) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.events = append(recorder.events, event)
}
func (recorder *eventRecorder) snapshot() []string {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([]string(nil), recorder.events...)
}

func assertEventOrder(t *testing.T, events []string, before, after string) {
	t.Helper()
	beforeIndex := slices.Index(events, before)
	afterIndex := slices.Index(events, after)
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf("event %q must precede %q: %v", before, after, events)
	}
}
