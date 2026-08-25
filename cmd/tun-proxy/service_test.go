package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
)

func TestServiceRunRequiresInstalledConfigPath(t *testing.T) {
	err := serviceRunCommand([]string{"-config", "/tmp/config.yaml"})
	if err == nil || !strings.Contains(err.Error(), "managed service requires") {
		t.Fatalf("serviceRunCommand() error = %v", err)
	}
}

func TestAbsoluteRegularSourceRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	real := filepath.Join(directory, "real")
	if err := os.WriteFile(real, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := absoluteRegularSource(link); err == nil {
		t.Fatal("symlink source was accepted")
	}
}

func TestValidateConfigSourceAcceptsCanonicalConfig(t *testing.T) {
	path, err := validateConfigSource("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("validated path is not absolute: %q", path)
	}
}

func TestServiceCommandRejectsUnknownSubcommand(t *testing.T) {
	err := serviceCommand([]string{"unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown service command") {
		t.Fatalf("serviceCommand() error = %v", err)
	}
}

type serviceStarterFunc func(context.Context) error

func (start serviceStarterFunc) Start(ctx context.Context) error {
	return start(ctx)
}

func TestServiceStartCommandAddsLogsHintOnStartFailure(t *testing.T) {
	want := errors.New("service did not become ready after 20s")
	err := serviceStartCommand(context.Background(), serviceStarterFunc(func(context.Context) error {
		return want
	}), nil)
	if !errors.Is(err, want) {
		t.Fatalf("serviceStartCommand() error = %v, want wrapped %v", err, want)
	}
	if !strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("serviceStartCommand() error = %q, want command %q", err, serviceLogsHintCommand)
	}
}

func TestServiceStartCommandRejectsArgumentsWithoutLogsHint(t *testing.T) {
	started := false
	err := serviceStartCommand(context.Background(), serviceStarterFunc(func(context.Context) error {
		started = true
		return nil
	}), []string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
		t.Fatalf("serviceStartCommand() error = %v", err)
	}
	if started {
		t.Fatal("service was started before rejecting arguments")
	}
	if strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("argument error unexpectedly included logs hint: %q", err)
	}
}

type serviceStartConfigManagerStub struct {
	startCalls int
	syncCalls  [][]byte
	start      func() error
	sync       func([]byte) (launchservice.ConfigSyncResult, error)
}

func (stub *serviceStartConfigManagerStub) Start(context.Context) error {
	stub.startCalls++
	if stub.start == nil {
		return nil
	}
	return stub.start()
}

func (stub *serviceStartConfigManagerStub) SynchronizeConfig(_ context.Context, contents []byte) (launchservice.ConfigSyncResult, error) {
	stub.syncCalls = append(stub.syncCalls, append([]byte(nil), contents...))
	if stub.sync == nil {
		return launchservice.ConfigSyncResult{}, nil
	}
	return stub.sync(contents)
}

func writeServiceStartConfig(t *testing.T, home, contents string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "tun-proxy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServiceStartWithConfigSynchronizesBeforeStarting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeServiceStartConfig(t, home, string(contents))

	manager := &serviceStartConfigManagerStub{}
	var order []string
	manager.sync = func(got []byte) (launchservice.ConfigSyncResult, error) {
		order = append(order, "sync")
		if !bytes.Equal(got, contents) {
			t.Fatalf("synchronized contents differ from user config")
		}
		return launchservice.ConfigSyncResult{}, nil
	}
	manager.start = func() error {
		order = append(order, "start")
		return nil
	}

	err = serviceStartWithConfigCommand(context.Background(), manager, filepath.Join(t.TempDir(), "managed.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "sync,start"; got != want {
		t.Fatalf("start order = %q, want %q", got, want)
	}
}

func TestServiceStartWithConfigAbortsWhenUserConfigIsInvalid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	writeServiceStartConfig(t, home, "version: 1\nnot_a_config: true\n")

	manager := &serviceStartConfigManagerStub{}
	err := serviceStartWithConfigCommand(context.Background(), manager, filepath.Join(t.TempDir(), "managed.yaml"), nil)
	if err == nil || !strings.Contains(err.Error(), "validate user configuration before service start") {
		t.Fatalf("serviceStartWithConfigCommand() error = %v", err)
	}
	if manager.startCalls != 0 || len(manager.syncCalls) != 0 {
		t.Fatalf("service started/synchronized after invalid user config: starts=%d syncs=%d", manager.startCalls, len(manager.syncCalls))
	}
}

func TestServiceStartWithConfigCanUseLastManagedConfigAfterUserValidationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	writeServiceStartConfig(t, home, "version: 1\nnot_a_config: true\n")
	managedPath := filepath.Join(t.TempDir(), "managed.yaml")
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &serviceStartConfigManagerStub{}

	err = serviceStartWithConfigCommand(context.Background(), manager, managedPath, []string{"-use-last-config"})
	if err != nil {
		t.Fatalf("serviceStartWithConfigCommand() error = %v", err)
	}
	if manager.startCalls != 1 || len(manager.syncCalls) != 0 {
		t.Fatalf("unexpected fallback calls: starts=%d syncs=%d", manager.startCalls, len(manager.syncCalls))
	}
}

func TestServiceStartWithConfigCanUseLastManagedConfigAfterSyncFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeServiceStartConfig(t, home, string(contents))
	managedPath := filepath.Join(t.TempDir(), "managed.yaml")
	if err := os.WriteFile(managedPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	wantSyncErr := errors.New("sync failed")
	manager := &serviceStartConfigManagerStub{}
	manager.sync = func(_ []byte) (launchservice.ConfigSyncResult, error) {
		if len(manager.syncCalls) == 1 {
			return launchservice.ConfigSyncResult{}, wantSyncErr
		}
		return launchservice.ConfigSyncResult{}, nil
	}

	err = serviceStartWithConfigCommand(context.Background(), manager, managedPath, []string{"-use-last-config"})
	if err != nil {
		t.Fatalf("serviceStartWithConfigCommand() error = %v", err)
	}
	if manager.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", manager.startCalls)
	}
	if len(manager.syncCalls) != 2 {
		t.Fatalf("sync calls = %d, want failed user sync plus managed-config restore", len(manager.syncCalls))
	}
}

func TestServiceStartWithConfigCanUseLastManagedConfigAfterStartFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeServiceStartConfig(t, home, string(contents))
	managedPath := filepath.Join(t.TempDir(), "managed.yaml")
	if err := os.WriteFile(managedPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	wantStartErr := errors.New("start failed")
	manager := &serviceStartConfigManagerStub{}
	manager.start = func() error {
		if manager.startCalls == 1 {
			return wantStartErr
		}
		return nil
	}

	err = serviceStartWithConfigCommand(context.Background(), manager, managedPath, []string{"-use-last-config"})
	if err != nil {
		t.Fatalf("serviceStartWithConfigCommand() error = %v", err)
	}
	if manager.startCalls != 2 || len(manager.syncCalls) != 2 {
		t.Fatalf("unexpected fallback calls: starts=%d syncs=%d", manager.startCalls, len(manager.syncCalls))
	}
}

func TestServiceStartWithConfigAbortsAfterStartFailureWithoutFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeServiceStartConfig(t, home, string(contents))
	wantStartErr := errors.New("start failed")
	manager := &serviceStartConfigManagerStub{start: func() error { return wantStartErr }}

	err = serviceStartWithConfigCommand(context.Background(), manager, filepath.Join(t.TempDir(), "managed.yaml"), nil)
	if !errors.Is(err, wantStartErr) {
		t.Fatalf("serviceStartWithConfigCommand() error = %v, want %v", err, wantStartErr)
	}
	if len(manager.syncCalls) != 1 || manager.startCalls != 1 {
		t.Fatalf("unexpected calls after start failure: syncs=%d starts=%d", len(manager.syncCalls), manager.startCalls)
	}
}

type serviceStopperFunc func(context.Context) error

func (stop serviceStopperFunc) Stop(ctx context.Context) error {
	return stop(ctx)
}

func TestServiceStopCommandAddsLogsHintOnStopFailure(t *testing.T) {
	want := errors.New("service did not stop cleanly")
	err := serviceStopCommand(context.Background(), serviceStopperFunc(func(context.Context) error {
		return want
	}), nil)
	if !errors.Is(err, want) {
		t.Fatalf("serviceStopCommand() error = %v, want wrapped %v", err, want)
	}
	if !strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("serviceStopCommand() error = %q, want command %q", err, serviceLogsHintCommand)
	}
}

func TestServiceStopCommandRejectsArgumentsWithoutLogsHint(t *testing.T) {
	stopped := false
	err := serviceStopCommand(context.Background(), serviceStopperFunc(func(context.Context) error {
		stopped = true
		return nil
	}), []string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
		t.Fatalf("serviceStopCommand() error = %v", err)
	}
	if stopped {
		t.Fatal("service was stopped before rejecting arguments")
	}
	if strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("argument error unexpectedly included logs hint: %q", err)
	}
}

func TestServiceInstallCommandAddsLogsHintToArgumentError(t *testing.T) {
	err := serviceInstallCommand(context.Background(), nil, []string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("serviceInstallCommand() error = %v", err)
	}
	if !strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("serviceInstallCommand() error = %q, want command %q", err, serviceLogsHintCommand)
	}
}

func TestWithServiceLogsHintAddsCommandAndPreservesCause(t *testing.T) {
	want := errors.New("install failed")
	got := withServiceLogsHint(want)
	if !errors.Is(got, want) {
		t.Fatalf("withServiceLogsHint() error = %v, want wrapped %v", got, want)
	}
	if !strings.Contains(got.Error(), serviceLogsHintCommand) {
		t.Fatalf("withServiceLogsHint() error = %q, want command %q", got, serviceLogsHintCommand)
	}
}

type serviceUpgraderFunc func(context.Context, string, string, *bool) (launchservice.UpgradeResult, error)

func (upgrade serviceUpgraderFunc) Upgrade(ctx context.Context, binary, config string, startAtBoot *bool) (launchservice.UpgradeResult, error) {
	return upgrade(ctx, binary, config, startAtBoot)
}

func TestServiceUpgradeCommandAddsLogsHintOnUpgradeFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "tun-proxy")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := errors.New("service did not become ready after 20s")
	err := serviceUpgradeCommand(context.Background(), serviceUpgraderFunc(func(context.Context, string, string, *bool) (launchservice.UpgradeResult, error) {
		return launchservice.UpgradeResult{}, want
	}), []string{"-binary", binary})
	if !errors.Is(err, want) {
		t.Fatalf("serviceUpgradeCommand() error = %v, want wrapped %v", err, want)
	}
	if !strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("serviceUpgradeCommand() error = %q, want command %q", err, serviceLogsHintCommand)
	}
}

func TestServiceUpgradeCommandRejectsArgumentsWithoutLogsHint(t *testing.T) {
	upgraded := false
	err := serviceUpgradeCommand(context.Background(), serviceUpgraderFunc(func(context.Context, string, string, *bool) (launchservice.UpgradeResult, error) {
		upgraded = true
		return launchservice.UpgradeResult{}, nil
	}), []string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("serviceUpgradeCommand() error = %v", err)
	}
	if upgraded {
		t.Fatal("service was upgraded before rejecting arguments")
	}
	if strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("argument error unexpectedly included logs hint: %q", err)
	}
}

func TestServiceUpgradeSuccessMessageReflectsRestart(t *testing.T) {
	tests := []struct {
		name   string
		result launchservice.UpgradeResult
		want   string
	}{
		{name: "restarted", result: launchservice.UpgradeResult{Restarted: true}, want: "tun-proxy service upgraded and restarted"},
		{name: "stopped", result: launchservice.UpgradeResult{}, want: "tun-proxy service upgraded; service remains stopped (startup not verified)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := serviceUpgradeSuccessMessage(test.result); got != test.want {
				t.Fatalf("serviceUpgradeSuccessMessage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWithServiceLogsHintSkipsHelpAndDuplicateHint(t *testing.T) {
	if got := withServiceLogsHint(flag.ErrHelp); !errors.Is(got, flag.ErrHelp) || strings.Contains(got.Error(), serviceLogsHintCommand) {
		t.Fatalf("withServiceLogsHint(flag.ErrHelp) = %v", got)
	}

	once := withServiceLogsHint(errors.New("install failed"))
	twice := withServiceLogsHint(once)
	if count := strings.Count(twice.Error(), serviceLogsHintCommand); count != 1 {
		t.Fatalf("service logs hint count = %d, want 1: %q", count, twice)
	}
}

func TestServiceWorkerInvocationRejectsArgumentsBeforeIdentityLookup(t *testing.T) {
	tests := [][]string{
		{"-config", "/tmp/config.yaml"},
		{"-uid", "501"},
		{"-gid", "20"},
		{"/tmp/control.sock"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			resolved := false
			err := validateServiceWorkerInvocation(
				args,
				func() (privsep.Identity, error) {
					resolved = true
					return privsep.Identity{}, nil
				},
				func(privsep.Identity) error { return nil },
			)
			if err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
				t.Fatalf("validateServiceWorkerInvocation(%q) error = %v", args, err)
			}
			if resolved {
				t.Fatal("worker identity was resolved before rejecting arguments")
			}
		})
	}
}

func TestServiceWorkerInvocationRejectsWrongCurrentIdentity(t *testing.T) {
	want := errors.New("worker credential mismatch")
	identity := privsep.Identity{
		User: privsep.ProductionUser, Group: privsep.ProductionGroup,
		UID: 499, GID: 499, Home: privsep.ProductionHome,
	}
	err := validateServiceWorkerInvocation(
		nil,
		func() (privsep.Identity, error) { return identity, nil },
		func(got privsep.Identity) error {
			if got != identity {
				t.Fatalf("validated identity = %+v, want %+v", got, identity)
			}
			return want
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("validateServiceWorkerInvocation() error = %v, want %v", err, want)
	}
}

func TestWriteManagedServiceLogPrefixesEveryLineWithLocalTimestamp(t *testing.T) {
	now := time.Date(2026, time.August, 20, 18, 7, 6, 123456789, time.Local)
	var output bytes.Buffer
	writeManagedServiceLog(&output, now, "first line\nsecond line\n")
	want := "2026-08-20T18:07:06.123" + now.Format("-07:00") + " first line\n" +
		"2026-08-20T18:07:06.123" + now.Format("-07:00") + " second line\n"
	if got := output.String(); got != want {
		t.Fatalf("managed service log = %q, want %q", got, want)
	}
}

func TestManagedServiceProcessDetection(t *testing.T) {
	for _, args := range [][]string{{"_service-run"}, {"_service-worker"}} {
		if !isManagedServiceProcess(args) {
			t.Fatalf("isManagedServiceProcess(%q) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"service", "start"}, {"run"}} {
		if isManagedServiceProcess(args) {
			t.Fatalf("isManagedServiceProcess(%q) = true", args)
		}
	}
}

func TestServiceOperationKindCoversEveryManagedMutation(t *testing.T) {
	want := map[string]launchservice.OperationKind{
		"install":   launchservice.OperationInstall,
		"start":     launchservice.OperationStart,
		"stop":      launchservice.OperationStop,
		"restart":   launchservice.OperationRestart,
		"reload":    launchservice.OperationReload,
		"upgrade":   launchservice.OperationUpgrade,
		"uninstall": launchservice.OperationUninstall,
	}
	for command, wantKind := range want {
		got, ok := serviceOperationKind(command)
		if !ok || got != wantKind {
			t.Errorf("serviceOperationKind(%q) = %q, %t; want %q, true", command, got, ok, wantKind)
		}
	}
	for _, command := range []string{"status", "logs", "help", "unknown", "sync-user-config"} {
		if got, ok := serviceOperationKind(command); ok {
			t.Errorf("serviceOperationKind(%q) = %q, true; want no lock", command, got)
		}
	}
}

func TestRemovedSyncUserConfigCommandIsUnknown(t *testing.T) {
	if err := serviceCommand([]string{"sync-user-config"}); err == nil || !strings.Contains(err.Error(), "unknown service command") {
		t.Fatalf("serviceCommand(sync-user-config) error = %v, want unknown command", err)
	}
}

func TestServiceOperationHelpBypassPredicate(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		if !hasOnlyHelpArgument([]string{arg}) {
			t.Errorf("hasOnlyHelpArgument(%q) = false", arg)
		}
	}
	if hasOnlyHelpArgument([]string{"-h", "extra"}) {
		t.Fatal("help plus extra arguments unexpectedly bypassed operation validation")
	}
}
