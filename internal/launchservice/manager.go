package launchservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hailinpan/tun-proxy/internal/daemon"
	"github.com/hailinpan/tun-proxy/internal/privsep"
	"github.com/hailinpan/tun-proxy/internal/system"
	"golang.org/x/sys/unix"
)

const launchctlPath = "/bin/launchctl"
const maxCommandOutput = 64 << 10

const serviceCommandTimeout = 20 * time.Second

const (
	InstallCommand   = "sudo tun-proxy service install"
	StartCommand     = "sudo tun-proxy service start"
	UpgradeCommand   = "sudo tun-proxy service upgrade"
	UninstallCommand = "sudo tun-proxy service uninstall"
)

var errJobNotLoaded = errors.New("launchd job is not loaded")

type CommandRunner interface {
	Run(ctx context.Context, executable string, args ...string) ([]byte, error)
}

type nativeRunner struct{}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (output *limitedBuffer) Write(contents []byte) (int, error) {
	written := len(contents)
	available := output.remaining
	if output.remaining > 0 {
		kept := contents
		if len(kept) > output.remaining {
			kept = kept[:output.remaining]
		}
		_, _ = output.buffer.Write(kept)
		output.remaining -= len(kept)
	}
	if len(contents) > available {
		output.truncated = true
	}
	return written, nil
}

func (output *limitedBuffer) Bytes() []byte { return output.buffer.Bytes() }

func (output *limitedBuffer) String() string {
	text := output.buffer.String()
	if output.truncated {
		text += "\n[output truncated]"
	}
	return text
}

func (nativeRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, serviceCommandTimeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, executable, args...)
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "LANG=C"}
	output := limitedBuffer{remaining: maxCommandOutput}
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(output.String())
		if len(args) >= 1 && args[0] == "print" && (strings.Contains(message, "Could not find service") || strings.Contains(message, "service not found")) {
			return output.Bytes(), fmt.Errorf("%w: %s", errJobNotLoaded, message)
		}
		return output.Bytes(), fmt.Errorf("run %s %s: %w: %s", executable, strings.Join(args, " "), err, message)
	}
	return output.Bytes(), nil
}

type RuntimeState struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Phase   string `json:"phase"`
}

type Status struct {
	Installed bool         `json:"installed"`
	Loaded    bool         `json:"loaded"`
	Runtime   RuntimeState `json:"runtime"`
}

type Manager struct {
	Layout       Layout
	Runner       CommandRunner
	Accounts     WorkerAccounts
	EffectiveUID func() int
	Probe        func() (RuntimeState, error)
	Recover      func(context.Context) error
	OwnerUID     int
	PollInterval time.Duration
	StartTimeout time.Duration
	StopTimeout  time.Duration
}

func NewManager(layout Layout) *Manager {
	runner := nativeRunner{}
	manager := &Manager{
		Layout: layout, Runner: runner, Accounts: newWorkerAccounts(runner), EffectiveUID: os.Geteuid, OwnerUID: 0,
		PollInterval: 100 * time.Millisecond, StartTimeout: 20 * time.Second,
		StopTimeout: 50 * time.Second,
	}
	manager.Probe = manager.probeRuntime
	return manager
}

func (manager *Manager) Install(ctx context.Context, binarySource, configSource string, start, startAtBoot bool) (resultErr error) {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	loaded, err := manager.loaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		return fmt.Errorf("launchd service label is already loaded; run %q first or choose a clean host", UninstallCommand)
	}
	identity, accountCreated, err := manager.Accounts.Ensure(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil && accountCreated {
			resultErr = errors.Join(resultErr, manager.Accounts.Purge(context.Background()))
		}
	}()
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(manager.Layout.Binary), 0o755},
		{filepath.Dir(manager.Layout.Config), 0o750},
		{filepath.Dir(manager.Layout.Plist), 0o755},
		{manager.Layout.LogDirectory, 0o750},
		{manager.Layout.RuntimeDir, 0o755},
	} {
		if err := ensureDirectory(directory.path, directory.mode, manager.OwnerUID); err != nil {
			return err
		}
	}
	storage, err := prepareWorkerStorage(manager.Layout, manager.OwnerUID, identity)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr == nil {
			storage.commit()
		} else {
			resultErr = errors.Join(resultErr, storage.rollback())
		}
	}()
	manifest, err := Manifest(manager.Layout, startAtBoot)
	if err != nil {
		return err
	}
	stages := make([]string, 0, 3)
	defer func() {
		for _, stage := range stages {
			_ = os.Remove(stage)
		}
	}()
	binaryStage, err := stageCopy(binarySource, manager.Layout.Binary, 0o755, maxInstalledFileSize)
	if err != nil {
		return err
	}
	stages = append(stages, binaryStage)
	configStage, err := stageCopy(configSource, manager.Layout.Config, 0o600, 1<<20)
	if err != nil {
		return err
	}
	stages = append(stages, configStage)
	plistStage, err := stageContents(manifest, manager.Layout.Plist, 0o644)
	if err != nil {
		return err
	}
	stages = append(stages, plistStage)

	replacements := make([]replacement, 0, 3)
	defer func() {
		if resultErr == nil {
			for index := range replacements {
				resultErr = errors.Join(resultErr, replacements[index].commit())
			}
			return
		}
		for index := len(replacements) - 1; index >= 0; index-- {
			resultErr = errors.Join(resultErr, replacements[index].rollback())
		}
	}()
	for _, item := range []struct{ stage, target string }{
		{binaryStage, manager.Layout.Binary}, {configStage, manager.Layout.Config}, {plistStage, manager.Layout.Plist},
	} {
		// Default uninstall deliberately preserves the managed configuration.
		// A later install must therefore be able to replace that one artifact,
		// while still refusing an unexpected executable or plist left behind.
		refuseExisting := item.target != manager.Layout.Config
		replaced, err := activateStage(item.stage, item.target, refuseExisting)
		if err != nil {
			return err
		}
		replacements = append(replacements, replaced)
	}
	bootstrapAttempted := false
	defer func() {
		if resultErr != nil && bootstrapAttempted {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if _, err := manager.Runner.Run(rollbackCtx, launchctlPath, "bootout", "system/"+manager.Layout.Label); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("rollback service load: %w", err))
			}
			if manager.Recover != nil {
				resultErr = errors.Join(resultErr, manager.Recover(rollbackCtx))
			}
		}
	}()
	if start {
		bootstrapAttempted = true
		if err := manager.Start(ctx); err != nil {
			return err
		}
		bootstrapAttempted = false
	}
	return nil
}

func serviceNotInstalledError() error {
	return fmt.Errorf("tun-proxy service is not completely installed; run %q first", InstallCommand)
}

func (manager *Manager) Start(ctx context.Context) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	installed, err := manager.installed()
	if err != nil {
		return err
	}
	if !installed {
		return serviceNotInstalledError()
	}
	if err := manager.bootstrap(ctx); err != nil {
		return err
	}
	if _, err := manager.Runner.Run(ctx, launchctlPath, "enable", "system/"+manager.Layout.Label); err != nil {
		return fmt.Errorf("enable service: %w", err)
	}
	_, kickstartErr := manager.Runner.Run(ctx, launchctlPath, "kickstart", "system/"+manager.Layout.Label)
	readyErr := manager.wait(ctx, manager.StartTimeout, func(state RuntimeState) bool {
		return state.Running && state.Phase == "running"
	}, "service did not become ready")
	if readyErr == nil {
		// launchctl can outlive the successful kickstart request and be killed by
		// the command timeout. Runtime readiness is authoritative once launchd
		// has published the new supervisor and worker state.
		return nil
	}
	if kickstartErr != nil {
		return errors.Join(fmt.Errorf("start service: %w", kickstartErr), readyErr)
	}
	return readyErr
}

func (manager *Manager) Stop(ctx context.Context) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	state, err := manager.Probe()
	if err != nil {
		return err
	}
	loaded, err := manager.loaded(ctx)
	if err != nil {
		return err
	}
	if state.Running && !loaded {
		return fmt.Errorf("runtime PID %d is alive but launchd job is not loaded", state.PID)
	}
	if loaded {
		if _, err := manager.Runner.Run(ctx, launchctlPath, "disable", "system/"+manager.Layout.Label); err != nil {
			return fmt.Errorf("disable service before stopping: %w", err)
		}
	}
	if !state.Running {
		if state.Phase != "" && manager.Recover != nil {
			return manager.Recover(ctx)
		}
		return nil
	}
	if _, err := manager.Runner.Run(ctx, launchctlPath, "kill", "SIGTERM", "system/"+manager.Layout.Label); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	return manager.wait(ctx, manager.StopTimeout, func(state RuntimeState) bool { return !state.Running && state.Phase == "" }, "service did not stop cleanly")
}

// Restart performs the same clean shutdown and readiness-checked startup used
// by the standalone stop and start commands. A stopped installed service is
// simply started.
func (manager *Manager) Restart(ctx context.Context) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	installed, err := manager.installed()
	if err != nil {
		return err
	}
	if !installed {
		return serviceNotInstalledError()
	}
	state, err := manager.Probe()
	if err != nil {
		return err
	}
	if state.Running || state.Phase != "" {
		if err := manager.Stop(ctx); err != nil {
			return err
		}
	}
	return manager.Start(ctx)
}

// Reload asks the running supervisor to re-read and atomically apply its
// managed configuration. Runtime acknowledgement is observed by the CLI via
// the worker status socket.
func (manager *Manager) Reload(ctx context.Context) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	installed, err := manager.installed()
	if err != nil {
		return err
	}
	if !installed {
		return serviceNotInstalledError()
	}
	state, err := manager.Probe()
	if err != nil {
		return err
	}
	if !state.Running || state.Phase != "running" {
		return fmt.Errorf("tun-proxy service is not running (phase=%q); run %q first", state.Phase, StartCommand)
	}
	loaded, err := manager.loaded(ctx)
	if err != nil {
		return err
	}
	if !loaded {
		return errors.New("tun-proxy service is running but the launchd job is not loaded")
	}
	if _, err := manager.Runner.Run(ctx, launchctlPath, "kill", "SIGHUP", "system/"+manager.Layout.Label); err != nil {
		return fmt.Errorf("reload service: %w", err)
	}
	return nil
}

func (manager *Manager) Upgrade(ctx context.Context, binarySource, configSource string, startAtBoot *bool) (resultErr error) {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	installed, err := manager.installed()
	if err != nil {
		return err
	}
	if !installed {
		return serviceNotInstalledError()
	}
	before, err := manager.Probe()
	if err != nil {
		return err
	}
	wasLoaded, err := manager.loaded(ctx)
	if err != nil {
		return err
	}
	binaryStage, err := stageCopy(binarySource, manager.Layout.Binary, 0o755, maxInstalledFileSize)
	if err != nil {
		return err
	}
	defer os.Remove(binaryStage) //nolint:errcheck // Best-effort staging cleanup.
	type stagedTarget struct{ stage, target string }
	staged := []stagedTarget{{binaryStage, manager.Layout.Binary}}
	if configSource != "" {
		configStage, err := stageCopy(configSource, manager.Layout.Config, 0o600, 1<<20)
		if err != nil {
			return err
		}
		defer os.Remove(configStage) //nolint:errcheck // Best-effort staging cleanup.
		staged = append(staged, stagedTarget{configStage, manager.Layout.Config})
	}
	effectiveStartAtBoot := false
	if startAtBoot == nil {
		contents, err := os.ReadFile(manager.Layout.Plist)
		if err != nil {
			return fmt.Errorf("read installed launchd manifest: %w", err)
		}
		effectiveStartAtBoot, err = ManifestStartAtBoot(contents)
		if err != nil {
			return err
		}
	} else {
		effectiveStartAtBoot = *startAtBoot
	}
	manifest, err := Manifest(manager.Layout, effectiveStartAtBoot)
	if err != nil {
		return err
	}
	plistStage, err := stageContents(manifest, manager.Layout.Plist, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(plistStage) //nolint:errcheck // Best-effort staging cleanup.
	staged = append(staged, stagedTarget{plistStage, manager.Layout.Plist})
	if before.Running || before.Phase != "" {
		if err := manager.Stop(ctx); err != nil {
			return err
		}
	}
	if wasLoaded {
		if _, err := manager.Runner.Run(ctx, launchctlPath, "bootout", "system/"+manager.Layout.Label); err != nil {
			return errors.Join(fmt.Errorf("unload service for upgrade: %w", err), manager.restoreServiceState(before, wasLoaded))
		}
	}
	if err := ensureDirectory(manager.Layout.RuntimeDir, 0o755, manager.OwnerUID); err != nil {
		return errors.Join(err, manager.restoreServiceState(before, wasLoaded))
	}
	identity, accountCreated, err := manager.Accounts.Ensure(ctx)
	if err != nil {
		return errors.Join(err, manager.restoreServiceState(before, wasLoaded))
	}
	storage, err := prepareWorkerStorage(manager.Layout, manager.OwnerUID, identity)
	if err != nil {
		if accountCreated {
			err = errors.Join(err, manager.Accounts.Purge(context.Background()))
		}
		return errors.Join(err, manager.restoreServiceState(before, wasLoaded))
	}
	replacements := make([]replacement, 0, len(staged))
	rollback := func() error {
		var failures []error
		for index := len(replacements) - 1; index >= 0; index-- {
			failures = append(failures, replacements[index].rollback())
		}
		return errors.Join(failures...)
	}
	rollbackAndRestore := func(cause error) error {
		failures := []error{cause}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), manager.StartTimeout+manager.StopTimeout+25*time.Second)
		defer cancel()
		loaded, err := manager.loaded(rollbackCtx)
		if err != nil {
			failures = append(failures, err)
		} else if loaded {
			if _, err := manager.Runner.Run(rollbackCtx, launchctlPath, "bootout", "system/"+manager.Layout.Label); err != nil {
				failures = append(failures, fmt.Errorf("unload failed upgrade: %w", err))
			}
		}
		if manager.Recover != nil {
			failures = append(failures, manager.Recover(rollbackCtx))
		}
		failures = append(failures, rollback())
		failures = append(failures, storage.rollback())
		if accountCreated {
			failures = append(failures, manager.Accounts.Purge(rollbackCtx))
		}
		failures = append(failures, manager.restoreServiceState(before, wasLoaded))
		return errors.Join(failures...)
	}
	for _, item := range staged {
		replaced, err := activateStage(item.stage, item.target, false)
		if err != nil {
			return rollbackAndRestore(err)
		}
		replacements = append(replacements, replaced)
	}
	if err := manager.restoreServiceState(before, wasLoaded); err != nil {
		return rollbackAndRestore(err)
	}
	for index := range replacements {
		resultErr = errors.Join(resultErr, replacements[index].commit())
	}
	storage.commit()
	return resultErr
}

func (manager *Manager) Uninstall(ctx context.Context, purge bool) (resultErr error) {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.requireRoot(); err != nil {
		return err
	}
	before, err := manager.Probe()
	if err != nil {
		return err
	}
	wasLoaded, err := manager.loaded(ctx)
	if err != nil {
		return err
	}
	if err := manager.Stop(ctx); err != nil {
		return err
	}
	if wasLoaded {
		if _, err := manager.Runner.Run(ctx, launchctlPath, "bootout", "system/"+manager.Layout.Label); err != nil {
			return errors.Join(fmt.Errorf("unload service: %w", err), manager.restoreServiceState(before, wasLoaded))
		}
	}
	if manager.Recover != nil {
		if err := manager.Recover(ctx); err != nil {
			return errors.Join(fmt.Errorf("recover service state before uninstall: %w", err), manager.restoreServiceState(before, wasLoaded))
		}
	}
	targets := []string{manager.Layout.Plist, manager.Layout.Binary}
	if purge {
		if _, err = manager.Accounts.Resolve(ctx); err != nil {
			return errors.Join(fmt.Errorf("validate dedicated worker identity before purge: %w", err), manager.restoreServiceState(before, wasLoaded))
		}
		targets = append(targets, manager.Layout.Config, manager.Layout.FakeIPv4, manager.Layout.FakeIPv6,
			manager.Layout.StandardOut, manager.Layout.StandardErr)
	}
	removals := make([]removal, 0, len(targets))
	rollback := func() error {
		var failures []error
		for index := len(removals) - 1; index >= 0; index-- {
			failures = append(failures, removals[index].rollback())
		}
		failures = append(failures, manager.restoreServiceState(before, wasLoaded))
		return errors.Join(failures...)
	}
	for _, target := range targets {
		removed, err := stageRemoval(target)
		if err != nil {
			return errors.Join(err, rollback())
		}
		removals = append(removals, removed)
	}
	if purge {
		if err := manager.Accounts.Purge(ctx); err != nil {
			return errors.Join(err, rollback())
		}
	}
	// All managed targets are now hidden at tombstone paths and the optional
	// worker identity has been purged. From this point onward uninstall is
	// committed: deleting tombstones is irreversible cleanup, so failures must
	// not attempt to restore only the subset that still exists.
	if err := commitRemovals(removals); err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("clean committed uninstall artifacts: %w", err))
	}
	if purge {
		for _, directory := range []string{manager.Layout.WorkerDir, manager.Layout.RuntimeDir, manager.Layout.DataDir, manager.Layout.LogDirectory, filepath.Dir(manager.Layout.Config)} {
			if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove empty service directory %q: %w", directory, err))
			}
		}
	}
	return resultErr
}

func (manager *Manager) Status(ctx context.Context) (Status, error) {
	if err := manager.validate(); err != nil {
		return Status{}, err
	}
	if err := manager.requireRoot(); err != nil {
		return Status{}, err
	}
	installed, err := manager.installed()
	if err != nil {
		return Status{}, err
	}
	if installed {
		identity, err := manager.Accounts.Resolve(ctx)
		if err != nil {
			return Status{}, fmt.Errorf("validate dedicated worker identity: %w", err)
		}
		if err := validateWorkerStorage(manager.Layout, identity); err != nil {
			return Status{}, err
		}
	}
	runtime, err := manager.Probe()
	if err != nil {
		return Status{}, err
	}
	loaded, err := manager.loaded(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: installed, Loaded: loaded, Runtime: runtime}, nil
}

func (manager *Manager) bootstrap(ctx context.Context) error {
	loaded, err := manager.loaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		return nil
	}
	if _, err := manager.Runner.Run(ctx, launchctlPath, "bootstrap", "system", manager.Layout.Plist); err != nil {
		return fmt.Errorf("load service: %w", err)
	}
	return nil
}

func (manager *Manager) loaded(ctx context.Context) (bool, error) {
	_, err := manager.Runner.Run(ctx, launchctlPath, "print", "system/"+manager.Layout.Label)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errJobNotLoaded) {
		return false, nil
	}
	return false, fmt.Errorf("inspect launchd service: %w", err)
}

func (manager *Manager) installed() (bool, error) {
	present := 0
	total := 0
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{manager.Layout.Binary, 0o755}, {manager.Layout.Config, 0o600}, {manager.Layout.Plist, 0o644},
	} {
		total++
		info, err := os.Lstat(item.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect service artifact %q: %w", item.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != item.mode {
			return false, fmt.Errorf("service artifact %q has unsafe type or mode %04o", item.path, info.Mode().Perm())
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != manager.OwnerUID {
			return false, fmt.Errorf("service artifact %q is not owned by UID %d", item.path, manager.OwnerUID)
		}
		present++
	}
	return present == total, nil
}

func (manager *Manager) probeRuntime() (RuntimeState, error) {
	state, err := system.ReadState(manager.Layout.State)
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeState{}, nil
	}
	if err != nil {
		return RuntimeState{}, err
	}
	lockPath := state.LockFile
	if lockPath == "" {
		lockPath = manager.Layout.Lock
	}
	lockState, err := daemon.ProbeLock(lockPath, state.PID)
	if err != nil {
		return RuntimeState{}, fmt.Errorf("inspect service instance lock: %w", err)
	}
	if lockState == daemon.LockHeld {
		return RuntimeState{Running: true, PID: state.PID, Phase: state.Phase}, nil
	}
	if lockState == daemon.LockStale {
		return RuntimeState{Running: false, PID: state.PID, Phase: state.Phase}, nil
	}
	processErr := unix.Kill(state.PID, 0)
	running := processErr == nil || errors.Is(processErr, unix.EPERM)
	if !running && !errors.Is(processErr, unix.ESRCH) {
		return RuntimeState{}, fmt.Errorf("inspect service process %d: %w", state.PID, processErr)
	}
	return RuntimeState{Running: running, PID: state.PID, Phase: state.Phase}, nil
}

func (manager *Manager) wait(ctx context.Context, timeout time.Duration, ready func(RuntimeState) bool, timeoutMessage string) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(manager.PollInterval)
	defer ticker.Stop()
	for {
		state, err := manager.Probe()
		if err != nil {
			return err
		}
		if ready(state) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			phase := state.Phase
			if phase == "" {
				phase = "not-reported"
			}
			return fmt.Errorf("%s after %s (last state: running=%t pid=%d phase=%q)", timeoutMessage, timeout, state.Running, state.PID, phase)
		case <-ticker.C:
		}
	}
}

func (manager *Manager) validate() error {
	if manager == nil {
		return errors.New("service manager is required")
	}
	if err := manager.Layout.Validate(); err != nil {
		return err
	}
	if manager.Runner == nil || manager.Accounts == nil || manager.EffectiveUID == nil || manager.Probe == nil {
		return errors.New("service manager dependencies are incomplete")
	}
	if manager.PollInterval <= 0 || manager.StartTimeout <= 0 || manager.StopTimeout <= 0 {
		return errors.New("service manager timeouts must be positive")
	}
	if manager.OwnerUID < 0 {
		return errors.New("service artifact owner UID must be non-negative")
	}
	return nil
}

func (manager *Manager) restartService() error {
	timeout := manager.StartTimeout + 25*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return manager.Start(ctx)
}

func (manager *Manager) restoreServiceState(before RuntimeState, wasLoaded bool) error {
	if before.Running {
		return manager.restartService()
	}
	if !wasLoaded {
		return nil
	}
	// Restore the observable "loaded but stopped" state by letting the old
	// version become ready and then stopping it cleanly while leaving the
	// launchd job registered. This works for either RunAtLoad policy.
	timeout := manager.StartTimeout + manager.StopTimeout + 25*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("restore loaded service: %w", err)
	}
	if err := manager.Stop(ctx); err != nil {
		return fmt.Errorf("restore stopped service: %w", err)
	}
	return nil
}

func (manager *Manager) requireRoot() error {
	if uid := manager.EffectiveUID(); uid != 0 {
		return fmt.Errorf("root privileges are required for service management (effective UID is %d); run with sudo", uid)
	}
	return nil
}

func PrepareRuntimeDirectories(layout Layout, expectedUID int) error {
	if err := layout.Validate(); err != nil {
		return err
	}
	if err := ensureDirectory(layout.RuntimeDir, 0o755, expectedUID); err != nil {
		return err
	}
	return ensureDirectory(layout.DataDir, 0o700, expectedUID)
}

// PrepareSupervisorRuntime verifies or creates only the root-owned runtime
// parent. Worker-owned directories are installed transactionally and must not
// be recreated by the supervisor under root ownership.
func PrepareSupervisorRuntime(layout Layout, expectedUID int) error {
	if err := layout.Validate(); err != nil {
		return err
	}
	return ensureDirectory(layout.RuntimeDir, 0o755, expectedUID)
}

// ResolveWorkerIdentity validates the dedicated service account including its
// ownership marker, disabled password and fixed shell/home contract.
func ResolveWorkerIdentity(ctx context.Context) (privsep.Identity, error) {
	runner := nativeRunner{}
	return newWorkerAccounts(runner).Resolve(ctx)
}
