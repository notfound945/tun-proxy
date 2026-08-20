package launchservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/privsep"
	"github.com/hailinpan/tun-proxy/internal/system"
)

type fakeAccounts struct {
	identity privsep.Identity
	exists   bool
	purgeErr error
	onPurge  func()
}

func (accounts *fakeAccounts) Ensure(context.Context) (privsep.Identity, bool, error) {
	created := !accounts.exists
	accounts.exists = true
	return accounts.identity, created, nil
}

func (accounts *fakeAccounts) Resolve(context.Context) (privsep.Identity, error) {
	if !accounts.exists {
		return privsep.Identity{}, errors.New("dedicated worker identity is missing")
	}
	return accounts.identity, nil
}

func (accounts *fakeAccounts) Purge(context.Context) error {
	if accounts.purgeErr != nil {
		return accounts.purgeErr
	}
	accounts.exists = false
	if accounts.onPurge != nil {
		accounts.onPurge()
	}
	return nil
}

func (accounts *fakeAccounts) Restore(_ context.Context, identity privsep.Identity) error {
	accounts.identity = identity
	accounts.exists = true
	return nil
}

type fakeRunner struct {
	loaded    bool
	disabled  bool
	calls     []string
	fail      func(string) error
	onSuccess func(string)
}

func (runner *fakeRunner) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{executable}, args...), " ")
	runner.calls = append(runner.calls, call)
	if runner.fail != nil {
		if err := runner.fail(call); err != nil {
			return nil, err
		}
	}
	if len(args) > 0 {
		switch args[0] {
		case "print":
			if !runner.loaded {
				return nil, errJobNotLoaded
			}
		case "bootstrap":
			runner.loaded = true
		case "bootout":
			runner.loaded = false
		case "disable":
			runner.disabled = true
		case "enable":
			runner.disabled = false
		}
	}
	if runner.onSuccess != nil {
		runner.onSuccess(call)
	}
	return nil, nil
}

func TestInstallCreatesArtifactsAndStarts(t *testing.T) {
	layout := testLayout(t)
	binarySource := filepath.Join(filepath.Dir(layout.Binary), "../source-binary")
	configSource := filepath.Join(filepath.Dir(layout.Config), "../source-config")
	writeTestFile(t, filepath.Clean(binarySource), "binary-v1", 0o755)
	writeTestFile(t, filepath.Clean(configSource), "config-v1", 0o600)
	state := RuntimeState{}
	runner := &fakeRunner{}
	manager := testManager(layout, runner, &state)
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 42, Phase: "running"}
		}
	}
	if err := manager.Install(context.Background(), filepath.Clean(binarySource), filepath.Clean(configSource), true); err != nil {
		t.Fatal(err)
	}
	assertFile(t, layout.Binary, "binary-v1", 0o755)
	assertFile(t, layout.Config, "config-v1", 0o600)
	assertFileMode(t, layout.Plist, 0o644)
	wantCalls := []string{
		launchctlPath + " print system/" + layout.Label,
		launchctlPath + " print system/" + layout.Label,
		launchctlPath + " bootstrap system " + layout.Plist,
		launchctlPath + " enable system/" + layout.Label,
		launchctlPath + " kickstart system/" + layout.Label,
	}
	assertCalls(t, runner.calls, wantCalls)
	assertNoResidueTree(t, filepath.Dir(layout.Binary))
}

func TestInstallWithoutStartLeavesJobUnloaded(t *testing.T) {
	layout := testLayout(t)
	binarySource := filepath.Join(filepath.Dir(layout.Binary), "../source-binary")
	configSource := filepath.Join(filepath.Dir(layout.Config), "../source-config")
	writeTestFile(t, filepath.Clean(binarySource), "binary-v1", 0o755)
	writeTestFile(t, filepath.Clean(configSource), "config-v1", 0o600)
	state := RuntimeState{}
	runner := &fakeRunner{}
	manager := testManager(layout, runner, &state)
	if err := manager.Install(context.Background(), filepath.Clean(binarySource), filepath.Clean(configSource), false); err != nil {
		t.Fatal(err)
	}
	if runner.loaded || containsCall(runner.calls, " bootstrap ") || containsCall(runner.calls, " kickstart ") {
		t.Fatalf("install -start=false loaded the job: loaded=%t calls=%v", runner.loaded, runner.calls)
	}
	assertFile(t, layout.Binary, "binary-v1", 0o755)
	assertFile(t, layout.Config, "config-v1", 0o600)
}

func TestServiceOperationsWithoutInstallationSuggestCompleteInstallCommand(t *testing.T) {
	operations := map[string]func(*Manager) error{
		"start":   func(manager *Manager) error { return manager.Start(context.Background()) },
		"restart": func(manager *Manager) error { return manager.Restart(context.Background()) },
		"reload":  func(manager *Manager) error { return manager.Reload(context.Background()) },
		"upgrade": func(manager *Manager) error { return manager.Upgrade(context.Background(), "/unused/binary", "") },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			layout := testLayout(t)
			state := RuntimeState{}
			manager := testManager(layout, &fakeRunner{}, &state)
			err := operation(manager)
			if err == nil || !strings.Contains(err.Error(), InstallCommand) {
				t.Fatalf("operation error = %v, want complete install command %q", err, InstallCommand)
			}
		})
	}
}

func TestReloadStoppedServiceSuggestsCompleteStartCommand(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{}
	manager := testManager(layout, &fakeRunner{loaded: true}, &state)
	err := manager.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), StartCommand) {
		t.Fatalf("Reload() error = %v, want complete start command %q", err, StartCommand)
	}
}

func TestStartAcceptsKickstartCommandFailureAfterRuntimeBecomesReady(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{}
	runner := &fakeRunner{loaded: true}
	runner.fail = func(call string) error {
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 42, Phase: "running"}
			return errors.New("signal: killed")
		}
		return nil
	}
	manager := testManager(layout, runner, &state)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() rejected ready runtime after kickstart transport failure: %v", err)
	}
}

func TestStartReportsKickstartFailureWhenRuntimeNeverBecomesReady(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{}
	runner := &fakeRunner{loaded: true, fail: func(call string) error {
		if strings.Contains(call, " kickstart ") {
			return errors.New("signal: killed")
		}
		return nil
	}}
	manager := testManager(layout, runner, &state)
	err := manager.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signal: killed") || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestRestartStopsThenStartsRunningService(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{Running: true, PID: 42, Phase: "running"}
	runner := &fakeRunner{loaded: true}
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kill SIGTERM ") {
			state = RuntimeState{}
		}
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 43, Phase: "running"}
		}
	}
	manager := testManager(layout, runner, &state)
	if err := manager.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, " kill SIGTERM ") || !containsCall(runner.calls, " kickstart ") {
		t.Fatalf("restart calls = %v", runner.calls)
	}
	if state.PID != 43 || !state.Running {
		t.Fatalf("restart state = %+v", state)
	}
}

func TestReloadSignalsRunningLaunchdJob(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{Running: true, PID: 42, Phase: "running"}
	runner := &fakeRunner{loaded: true}
	manager := testManager(layout, runner, &state)
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, " kill SIGHUP system/"+layout.Label) {
		t.Fatalf("reload calls = %v", runner.calls)
	}
}

func TestInstallBootstrapFailureRollsBack(t *testing.T) {
	layout := testLayout(t)
	binarySource := filepath.Join(filepath.Dir(layout.Binary), "../source-binary")
	configSource := filepath.Join(filepath.Dir(layout.Config), "../source-config")
	writeTestFile(t, filepath.Clean(binarySource), "binary-v1", 0o755)
	writeTestFile(t, filepath.Clean(configSource), "config-v1", 0o600)
	state := RuntimeState{}
	recovered := 0
	runner := &fakeRunner{fail: func(call string) error {
		if strings.Contains(call, " bootstrap ") {
			return errors.New("bootstrap failed")
		}
		return nil
	}}
	manager := testManager(layout, runner, &state)
	manager.Recover = func(context.Context) error { recovered++; return nil }
	err := manager.Install(context.Background(), filepath.Clean(binarySource), filepath.Clean(configSource), true)
	if err == nil || !strings.Contains(err.Error(), "bootstrap failed") {
		t.Fatalf("Install() error = %v", err)
	}
	for _, path := range []string{layout.Binary, layout.Config, layout.Plist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("artifact remains after rollback %q: %v", path, err)
		}
	}
	if recovered != 1 {
		t.Fatalf("recovery calls = %d", recovered)
	}
	if !containsCall(runner.calls, " bootout ") {
		t.Fatal("failed bootstrap did not attempt bootout")
	}
	assertNoResidueTree(t, filepath.Dir(layout.Binary))
}

func TestInstallReplacesConfigPreservedByDefaultUninstall(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "old-binary", "preserved-config")
	state := RuntimeState{}
	runner := &fakeRunner{}
	manager := testManager(layout, runner, &state)
	manager.Recover = func(context.Context) error { return nil }
	if err := manager.Uninstall(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	binarySource := filepath.Join(filepath.Dir(layout.Binary), "../source-binary")
	configSource := filepath.Join(filepath.Dir(layout.Config), "../source-config")
	writeTestFile(t, filepath.Clean(binarySource), "new-binary", 0o755)
	writeTestFile(t, filepath.Clean(configSource), "new-config", 0o600)
	if err := manager.Install(context.Background(), filepath.Clean(binarySource), filepath.Clean(configSource), false); err != nil {
		t.Fatal(err)
	}
	assertFile(t, layout.Binary, "new-binary", 0o755)
	assertFile(t, layout.Config, "new-config", 0o600)
	assertFileMode(t, layout.Plist, 0o644)
}

func TestFailedReinstallRestoresPreservedConfig(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(filepath.Dir(layout.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, layout.Config, "preserved-config", 0o600)
	binarySource := filepath.Join(filepath.Dir(layout.Binary), "../source-binary")
	configSource := filepath.Join(filepath.Dir(layout.Config), "../source-config")
	writeTestFile(t, filepath.Clean(binarySource), "new-binary", 0o755)
	writeTestFile(t, filepath.Clean(configSource), "new-config", 0o600)
	state := RuntimeState{}
	runner := &fakeRunner{fail: func(call string) error {
		if strings.Contains(call, " bootstrap ") {
			return errors.New("bootstrap failed")
		}
		return nil
	}}
	manager := testManager(layout, runner, &state)
	manager.Recover = func(context.Context) error { return nil }
	if err := manager.Install(context.Background(), filepath.Clean(binarySource), filepath.Clean(configSource), true); err == nil {
		t.Fatal("failed reinstall unexpectedly succeeded")
	}
	assertFile(t, layout.Config, "preserved-config", 0o600)
	for _, path := range []string{layout.Binary, layout.Plist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("artifact remains after failed reinstall %q: %v", path, err)
		}
	}
}

func TestStopRecoversDeadStaleState(t *testing.T) {
	layout := testLayout(t)
	state := RuntimeState{Running: false, PID: 77, Phase: "running"}
	runner := &fakeRunner{}
	manager := testManager(layout, runner, &state)
	recovered := 0
	manager.Recover = func(context.Context) error {
		recovered++
		state = RuntimeState{}
		return nil
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovery calls = %d", recovered)
	}
}

func TestStopDisablesLoadedJobDuringCrashBackoff(t *testing.T) {
	layout := testLayout(t)
	state := RuntimeState{Running: false, PID: 77, Phase: "running"}
	runner := &fakeRunner{loaded: true}
	manager := testManager(layout, runner, &state)
	recovered := 0
	manager.Recover = func(context.Context) error {
		recovered++
		state = RuntimeState{}
		return nil
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runner.disabled || recovered != 1 {
		t.Fatalf("disabled=%t recovery calls=%d", runner.disabled, recovered)
	}
	if !containsCall(runner.calls, " disable system/"+layout.Label) {
		t.Fatalf("stop calls = %v", runner.calls)
	}
	if containsCall(runner.calls, " kill SIGTERM ") {
		t.Fatalf("crash-backoff stop tried to signal a dead process: %v", runner.calls)
	}
}

func TestProbeRuntimeUsesUnlockedStaleLockBeforeLivePID(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(layout.RuntimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Lock, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	state := system.NewState("test-digest")
	state.LockFile = layout.Lock
	state.Phase = "running"
	if err := system.WriteState(layout.State, state); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(layout)
	runtime, err := manager.probeRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Running || runtime.PID != os.Getpid() || runtime.Phase != "running" {
		t.Fatalf("probe runtime = %+v", runtime)
	}
}

func TestUpgradeRollsBackAndRestartsOldVersion(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "old-binary", "old-config")
	newBinary := filepath.Join(filepath.Dir(layout.Binary), "../new-binary")
	newConfig := filepath.Join(filepath.Dir(layout.Config), "../new-config")
	writeTestFile(t, filepath.Clean(newBinary), "new-binary", 0o755)
	writeTestFile(t, filepath.Clean(newConfig), "new-config", 0o600)
	state := RuntimeState{Running: true, PID: 88, Phase: "running"}
	kickstarts := 0
	runner := &fakeRunner{loaded: true}
	runner.fail = func(call string) error {
		if strings.Contains(call, " kickstart ") {
			kickstarts++
			if kickstarts == 1 {
				return errors.New("new version failed")
			}
		}
		return nil
	}
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kill ") {
			state = RuntimeState{}
		}
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 99, Phase: "running"}
		}
	}
	manager := testManager(layout, runner, &state)
	err := manager.Upgrade(context.Background(), filepath.Clean(newBinary), filepath.Clean(newConfig))
	if err == nil || !strings.Contains(err.Error(), "new version failed") {
		t.Fatalf("Upgrade() error = %v", err)
	}
	assertFile(t, layout.Binary, "old-binary", 0o755)
	assertFile(t, layout.Config, "old-config", 0o600)
	if !state.Running || kickstarts != 2 {
		t.Fatalf("old version was not restarted: state=%+v kickstarts=%d", state, kickstarts)
	}
	if countCalls(runner.calls, " bootout ") != 2 {
		t.Fatalf("failed new job was not unloaded before rollback: %v", runner.calls)
	}
	assertNoResidueTree(t, filepath.Dir(layout.Binary))
}

func TestUpgradeReplacesArtifactsAndRestarts(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "old-binary", "old-config")
	newBinary := filepath.Join(filepath.Dir(layout.Binary), "../new-binary")
	newConfig := filepath.Join(filepath.Dir(layout.Config), "../new-config")
	writeTestFile(t, filepath.Clean(newBinary), "new-binary", 0o755)
	writeTestFile(t, filepath.Clean(newConfig), "new-config", 0o600)
	state := RuntimeState{Running: true, PID: 88, Phase: "running"}
	runner := &fakeRunner{loaded: true}
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kill ") {
			state = RuntimeState{}
		}
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 99, Phase: "running"}
		}
	}
	manager := testManager(layout, runner, &state)
	if err := manager.Upgrade(context.Background(), filepath.Clean(newBinary), filepath.Clean(newConfig)); err != nil {
		t.Fatal(err)
	}
	assertFile(t, layout.Binary, "new-binary", 0o755)
	assertFile(t, layout.Config, "new-config", 0o600)
	if !state.Running || state.PID != 99 {
		t.Fatalf("new version was not restarted: state=%+v", state)
	}
	assertNoResidueTree(t, filepath.Dir(layout.Binary))
}

func TestUpgradeRestoresLoadedStoppedState(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "old-binary", "old-config")
	newBinary := filepath.Join(filepath.Dir(layout.Binary), "../new-binary")
	writeTestFile(t, filepath.Clean(newBinary), "new-binary", 0o755)
	state := RuntimeState{}
	runner := &fakeRunner{loaded: true}
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 99, Phase: "running"}
		}
		if strings.Contains(call, " kill ") {
			state = RuntimeState{}
		}
	}
	manager := testManager(layout, runner, &state)
	if err := manager.Upgrade(context.Background(), filepath.Clean(newBinary), ""); err != nil {
		t.Fatal(err)
	}
	assertFile(t, layout.Binary, "new-binary", 0o755)
	if !runner.loaded || state.Running {
		t.Fatalf("upgrade did not preserve loaded/stopped state: loaded=%t state=%+v", runner.loaded, state)
	}
	if !containsCall(runner.calls, " bootstrap ") || !containsCall(runner.calls, " kickstart ") || !containsCall(runner.calls, " kill ") {
		t.Fatalf("loaded/stopped upgrade did not reload and stop job: %v", runner.calls)
	}
}

func TestUninstallPreservesDataByDefault(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	if err := os.MkdirAll(layout.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.LogDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, layout.FakeIPv4, "mapping", 0o600)
	writeTestFile(t, layout.StandardOut, "log", 0o600)
	state := RuntimeState{Running: true, PID: 101, Phase: "running"}
	runner := &fakeRunner{loaded: true}
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kill ") {
			state = RuntimeState{}
		}
	}
	manager := testManager(layout, runner, &state)
	recovered := 0
	manager.Recover = func(context.Context) error { recovered++; return nil }
	if err := manager.Uninstall(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.Binary, layout.Plist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("uninstall artifact remains %q: %v", path, err)
		}
	}
	for _, path := range []string{layout.Config, layout.FakeIPv4, layout.StandardOut} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("preserved data missing %q: %v", path, err)
		}
	}
	if recovered != 1 || runner.loaded {
		t.Fatalf("uninstall recovery=%d loaded=%t", recovered, runner.loaded)
	}
}

func TestUninstallFailureRestoresLoadedStoppedService(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	if err := os.MkdirAll(layout.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A managed path with an unsafe type forces failure after the plist and
	// binary removals have already been staged.
	if err := os.Mkdir(layout.FakeIPv4, 0o700); err != nil {
		t.Fatal(err)
	}
	state := RuntimeState{}
	runner := &fakeRunner{loaded: true}
	runner.onSuccess = func(call string) {
		if strings.Contains(call, " kickstart ") {
			state = RuntimeState{Running: true, PID: 111, Phase: "running"}
		}
		if strings.Contains(call, " kill ") {
			state = RuntimeState{}
		}
	}
	manager := testManager(layout, runner, &state)
	manager.Recover = func(context.Context) error { return nil }
	err := manager.Uninstall(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "refuse to remove non-regular") {
		t.Fatalf("Uninstall() error = %v", err)
	}
	assertFile(t, layout.Binary, "binary", 0o755)
	assertFile(t, layout.Plist, "plist", 0o644)
	if !runner.loaded || state.Running {
		t.Fatalf("service state was not restored to loaded/stopped: loaded=%t state=%+v", runner.loaded, state)
	}
	if !containsCall(runner.calls, " kickstart ") || !containsCall(runner.calls, " kill ") {
		t.Fatalf("loaded/stopped restoration did not start and stop service: %v", runner.calls)
	}
}

func TestPurgeRemovesKnownFilesAndPreservesUnknownFiles(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	for _, directory := range []string{layout.DataDir, layout.LogDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{layout.FakeIPv4, layout.FakeIPv6, layout.StandardOut, layout.StandardErr} {
		writeTestFile(t, path, "managed", 0o600)
	}
	unknown := filepath.Join(layout.DataDir, "operator-note.txt")
	writeTestFile(t, unknown, "preserve", 0o600)
	state := RuntimeState{}
	runner := &fakeRunner{}
	manager := testManager(layout, runner, &state)
	manager.Recover = func(context.Context) error { return nil }
	if err := manager.Uninstall(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.Binary, layout.Config, layout.Plist, layout.FakeIPv4, layout.FakeIPv6, layout.StandardOut, layout.StandardErr} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("managed file remains %q: %v", path, err)
		}
	}
	assertFile(t, unknown, "preserve", 0o600)
}

func TestPurgeCleanupFailureLeavesServiceFullyUninstalled(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{}
	runner := &fakeRunner{loaded: true}
	manager := testManager(layout, runner, &state)
	manager.Recover = func(context.Context) error { return nil }
	accounts := manager.Accounts.(*fakeAccounts)
	var blockedTombstone string
	accounts.onPurge = func() {
		entries, err := os.ReadDir(filepath.Dir(layout.Binary))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".tun-proxy-remove-") {
				blockedTombstone = filepath.Join(filepath.Dir(layout.Binary), entry.Name())
				break
			}
		}
		if blockedTombstone == "" {
			t.Fatal("binary removal tombstone was not staged before account purge")
		}
		if err := os.Remove(blockedTombstone); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(blockedTombstone, 0o700); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(blockedTombstone, "residue"), "cleanup failed", 0o600)
	}

	err := manager.Uninstall(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "clean committed uninstall artifacts") {
		t.Fatalf("Uninstall() error = %v", err)
	}
	for _, target := range []string{layout.Plist, layout.Binary, layout.Config} {
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("committed uninstall restored target %q: %v", target, err)
		}
	}
	if runner.loaded {
		t.Fatal("cleanup failure reloaded the uninstalled service")
	}
	if accounts.exists {
		t.Fatal("cleanup failure restored the purged worker identity")
	}
	if info, err := os.Lstat(blockedTombstone); err != nil || !info.IsDir() {
		t.Fatalf("cleanup residue = %v, %v", info, err)
	}
	if containsCall(runner.calls, " bootstrap ") {
		t.Fatalf("cleanup failure attempted service restoration: %v", runner.calls)
	}
}

func TestPurgeAccountFailureRestoresStagedFiles(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{}
	manager := testManager(layout, &fakeRunner{}, &state)
	accounts := manager.Accounts.(*fakeAccounts)
	accounts.purgeErr = errors.New("directory service unavailable")
	manager.Recover = func(context.Context) error { return nil }
	err := manager.Uninstall(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "directory service unavailable") {
		t.Fatalf("Uninstall() error = %v", err)
	}
	assertFile(t, layout.Binary, "binary", 0o755)
	assertFile(t, layout.Config, "config", 0o600)
	assertFile(t, layout.Plist, "plist", 0o644)
	if !accounts.exists {
		t.Fatal("failed purge removed worker identity")
	}
}

func TestPurgeRemovesNestedWorkerRuntimeBeforeParent(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	state := RuntimeState{}
	manager := testManager(layout, &fakeRunner{}, &state)
	manager.Recover = func(context.Context) error { return nil }
	if err := manager.Uninstall(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.WorkerDir, layout.RuntimeDir, layout.DataDir} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("purged directory remains %q: %v", path, err)
		}
	}
}

func TestStatusRejectsUnsafeInstalledMode(t *testing.T) {
	layout := testLayout(t)
	createInstalledArtifacts(t, layout, "binary", "config")
	if err := os.Chmod(layout.Config, 0o644); err != nil {
		t.Fatal(err)
	}
	state := RuntimeState{}
	manager := testManager(layout, &fakeRunner{}, &state)
	if _, err := manager.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe type or mode") {
		t.Fatalf("Status() error = %v", err)
	}
}

func TestStatusRejectsUnsafeArtifactInPartialInstall(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(filepath.Dir(layout.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, layout.Config, "config", 0o644)
	state := RuntimeState{}
	manager := testManager(layout, &fakeRunner{}, &state)
	if _, err := manager.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe type or mode") {
		t.Fatalf("Status() error = %v", err)
	}
}

func TestLimitedBufferBoundsOutput(t *testing.T) {
	output := limitedBuffer{remaining: 4}
	if written, err := output.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if got := string(output.Bytes()); got != "abcd" {
		t.Fatalf("Bytes() = %q", got)
	}
	if got := output.String(); got != "abcd\n[output truncated]" {
		t.Fatalf("String() = %q", got)
	}
}

func TestManagerRequiresRoot(t *testing.T) {
	layout := testLayout(t)
	manager := NewManager(layout)
	manager.EffectiveUID = func() int { return 501 }
	manager.OwnerUID = os.Geteuid()
	manager.Runner = &fakeRunner{}
	manager.Probe = func() (RuntimeState, error) { return RuntimeState{}, nil }
	if _, err := manager.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "root privileges") {
		t.Fatalf("Status() error = %v", err)
	}
}

func TestPrepareRuntimeDirectoriesRecreatesExpectedModes(t *testing.T) {
	layout := testLayout(t)
	if err := PrepareRuntimeDirectories(layout, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	assertDirectoryMode(t, layout.RuntimeDir, 0o755)
	assertDirectoryMode(t, layout.DataDir, 0o700)
}

func TestPrepareSupervisorRuntimeDoesNotCreateWorkerOwnedDirectories(t *testing.T) {
	layout := testLayout(t)
	if err := PrepareSupervisorRuntime(layout, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	assertDirectoryMode(t, layout.RuntimeDir, 0o755)
	for _, path := range []string{layout.WorkerDir, layout.DataDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Lstat(%q) error = %v, want os.ErrNotExist", path, err)
		}
	}
}

func testLayout(t *testing.T) Layout {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "tun-proxy-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return Layout{
		Label:  "com.example.tun-proxy",
		Binary: filepath.Join(root, "bin", "tun-proxy"), Config: filepath.Join(root, "etc", "config.yaml"),
		Plist: filepath.Join(root, "LaunchDaemons", "com.example.tun-proxy.plist"), LogDirectory: filepath.Join(root, "logs"),
		StandardOut: filepath.Join(root, "logs", "stdout.log"), StandardErr: filepath.Join(root, "logs", "stderr.log"),
		RuntimeDir: filepath.Join(root, "run"), WorkerUser: "_tun-proxy", WorkerGroup: "_tun-proxy",
		WorkerDir: filepath.Join(root, "run", "worker"), StatusSocket: filepath.Join(root, "run", "worker", "status.sock"),
		DataDir: filepath.Join(root, "lib"), State: filepath.Join(root, "run", "state.json"),
		Lock: filepath.Join(root, "run", "lock"), FakeIPv4: filepath.Join(root, "lib", "fake-ip.yaml"),
		FakeIPv6: filepath.Join(root, "lib", "fake-ipv6.yaml"),
	}
}

func testManager(layout Layout, runner CommandRunner, state *RuntimeState) *Manager {
	manager := NewManager(layout)
	manager.Runner = runner
	manager.Accounts = &fakeAccounts{identity: privsep.Identity{
		User: layout.WorkerUser, Group: layout.WorkerGroup,
		UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()), Home: privsep.ProductionHome,
	}, exists: true}
	manager.EffectiveUID = func() int { return 0 }
	manager.OwnerUID = os.Geteuid()
	manager.PollInterval = time.Millisecond
	manager.StartTimeout = 100 * time.Millisecond
	manager.StopTimeout = 100 * time.Millisecond
	manager.Probe = func() (RuntimeState, error) { return *state, nil }
	return manager
}

func createInstalledArtifacts(t *testing.T, layout Layout, binary, config string) {
	t.Helper()
	for _, directory := range []string{filepath.Dir(layout.Binary), filepath.Dir(layout.Config), filepath.Dir(layout.Plist)} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, layout.Binary, binary, 0o755)
	writeTestFile(t, layout.Config, config, 0o600)
	writeTestFile(t, layout.Plist, "plist", 0o644)
	for _, directory := range []string{layout.RuntimeDir, layout.WorkerDir, layout.DataDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func assertFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if got := readTestFile(t, path); got != contents {
		t.Fatalf("%s contents = %q, want %q", path, got, contents)
	}
	assertFileMode(t, path, mode)
}

func assertFileMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), mode)
	}
}

func assertDirectoryMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %v, want directory %04o", path, info.Mode(), mode)
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func containsCall(calls []string, fragment string) bool {
	for _, call := range calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

func countCalls(calls []string, fragment string) int {
	count := 0
	for _, call := range calls {
		if strings.Contains(call, fragment) {
			count++
		}
	}
	return count
}

func assertNoResidueTree(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(filepath.Dir(root), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".tun-proxy-") {
			return fmt.Errorf("transaction residue remains: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
