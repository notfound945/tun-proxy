package system

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStateRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := NewState("sha256:test")
	want.Phase = "running"
	want.TUNName = "utun7"
	want.StatusSocket = "/var/run/tun-proxy/worker/status.sock"
	want.ControlSocket = "/var/run/tun-proxy/control.sock"
	want.Route = &RouteState{Prefix: "198.18.0.0/15", Interface: "utun7"}
	want.Routes = []RouteState{
		{Prefix: "fd00:7::/96", Interface: "utun7"},
		{Prefix: "1.1.1.1/32", Interface: "en0", Gateway: "192.0.2.1"},
		{Prefix: "0.0.0.0/1", Interface: "en0", Gateway: "192.0.2.1", Scope: "en0"},
	}

	if err := WriteState(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %04o, want 0600", got)
	}
	got, err := ReadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadState() = %#v, want %#v", got, want)
	}
}

func TestStateRejectsInvalidControlSocketPath(t *testing.T) {
	state := NewState("sha256:test")
	state.ControlSocket = "relative/control.sock"
	if err := WriteState(filepath.Join(t.TempDir(), "state.json"), state); err == nil || !strings.Contains(err.Error(), "control socket") {
		t.Fatalf("WriteState() error = %v", err)
	}
}

func TestStateRefusesSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(link, NewState("sha256:test")); err == nil {
		t.Fatal("WriteState accepted a symlink")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("symlink target changed to %q", contents)
	}
}

func TestTransactionRollsBackInReverseOrder(t *testing.T) {
	var events []string
	transaction := new(Transaction)
	for _, name := range []string{"state", "tun", "route"} {
		name := name
		if err := transaction.Apply(name, func() error {
			events = append(events, "apply "+name)
			return nil
		}, func() error {
			events = append(events, "undo "+name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	want := []string{"apply state", "apply tun", "apply route", "undo route", "undo tun", "undo state"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("second rollback = %v, want nil", err)
	}
}

func TestTransactionAggregatesRollbackFailures(t *testing.T) {
	transaction := new(Transaction)
	for _, name := range []string{"one", "two"} {
		name := name
		if err := transaction.Apply(name, func() error { return nil }, func() error { return errors.New(name) }); err != nil {
			t.Fatal(err)
		}
	}
	err := transaction.Rollback()
	if err == nil || !strings.Contains(err.Error(), "rollback two") || !strings.Contains(err.Error(), "rollback one") {
		t.Fatalf("Rollback() = %v, want aggregated error", err)
	}
}
