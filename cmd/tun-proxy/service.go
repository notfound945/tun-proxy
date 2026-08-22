package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/app"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	"github.com/hailinpan/tun-proxy/internal/system"
	"golang.org/x/sys/unix"
)

func serviceCommand(args []string) error {
	if len(args) == 0 {
		printServiceUsage()
		return errors.New("a service command is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	manager := newServiceManager()
	switch args[0] {
	case "install":
		return serviceInstallCommand(ctx, manager, args[1:])
	case "start":
		return serviceStartCommand(ctx, manager, args[1:])
	case "stop":
		return serviceStopCommand(ctx, manager, args[1:])
	case "restart":
		if hasOnlyHelpArgument(args[1:]) {
			return helpCommand([]string{"service", "restart"})
		}
		if len(args) != 1 {
			return errors.New("service restart does not accept arguments")
		}
		if err := manager.Restart(ctx); err != nil {
			return err
		}
		fmt.Println("tun-proxy service restarted")
		return nil
	case "sync-user-config":
		return serviceSyncUserConfigCommand(ctx, manager, args[1:])
	case "reload":
		return serviceReloadCommand(ctx, manager, args[1:])
	case "status":
		return serviceStatusCommand(ctx, manager, args[1:])
	case "logs":
		return serviceLogsCommand(ctx, manager.Layout, args[1:])
	case "upgrade":
		return serviceUpgradeCommand(ctx, manager, args[1:])
	case "uninstall":
		return serviceUninstallCommand(ctx, manager, args[1:])
	case "help", "-h", "--help":
		return helpCommand(append([]string{"service"}, args[1:]...))
	default:
		printServiceUsage()
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

func hasOnlyHelpArgument(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

type serviceStarter interface {
	Start(context.Context) error
}

func serviceStartCommand(ctx context.Context, starter serviceStarter, args []string) error {
	if hasOnlyHelpArgument(args) {
		return helpCommand([]string{"service", "start"})
	}
	if len(args) != 0 {
		return errors.New("service start does not accept arguments")
	}
	if err := starter.Start(ctx); err != nil {
		return withServiceLogsHint(err)
	}
	fmt.Println("tun-proxy service started")
	return nil
}

type serviceStopper interface {
	Stop(context.Context) error
}

func serviceStopCommand(ctx context.Context, stopper serviceStopper, args []string) error {
	if hasOnlyHelpArgument(args) {
		return helpCommand([]string{"service", "stop"})
	}
	if len(args) != 0 {
		return errors.New("service stop does not accept arguments")
	}
	if err := stopper.Stop(ctx); err != nil {
		return withServiceLogsHint(err)
	}
	fmt.Println("tun-proxy service stopped and unloaded")
	return nil
}

type serviceConfigSynchronizer interface {
	SynchronizeConfig(context.Context, []byte) (launchservice.ConfigSyncResult, error)
}

func serviceSyncUserConfigCommand(ctx context.Context, synchronizer serviceConfigSynchronizer, args []string) error {
	if hasOnlyHelpArgument(args) {
		return helpCommand([]string{"service", "sync-user-config"})
	}
	if len(args) != 0 {
		return errors.New("service sync-user-config does not accept arguments")
	}
	source := defaultUserConfigPath()
	source, contents, _, _, err := loadValidatedConfigSource(source)
	if err != nil {
		return fmt.Errorf("validate user configuration before synchronization: %w", err)
	}
	result, err := synchronizer.SynchronizeConfig(ctx, contents)
	if err != nil {
		return withServiceLogsHint(fmt.Errorf("synchronize user configuration: %w", err))
	}
	fmt.Printf("user configuration synchronized: %s\n", source)
	if result.Restarted {
		fmt.Println("tun-proxy service restarted with the synchronized configuration")
	} else {
		fmt.Printf("tun-proxy service remains stopped; run %q to start it\n", launchservice.StartCommand)
	}
	return nil
}

type serviceReloadOptions struct {
	timeout       time.Duration
	configPath    string
	useUserConfig bool
}

func newServiceReloadFlagSet(output io.Writer, options *serviceReloadOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceReloadOptions{}
	}
	flags := newCommandFlagSet("service reload", output)
	flags.DurationVar(&options.timeout, "timeout", 15*time.Second, "wait `DURATION` for runtime confirmation")
	flags.StringVar(&options.configPath, "config", "", "validate and install configuration `PATH` before reload")
	flags.BoolVar(&options.useUserConfig, "user-config", false, "reload the invoking user's default configuration")
	return flags
}

func serviceReloadCommand(ctx context.Context, manager *launchservice.Manager, args []string) (resultErr error) {
	options := serviceReloadOptions{}
	flags := newServiceReloadFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service reload received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if options.timeout <= 0 {
		return errors.New("service reload timeout must be positive")
	}
	configPath, err := resolveServiceReloadConfigPath(options)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withServiceLogsHint(resultErr)
	}()

	status, err := manager.Status(ctx)
	if err != nil {
		return fmt.Errorf("inspect service before reload: %w", err)
	}
	if err := validateServiceReloadStatus(status); err != nil {
		return err
	}

	var configContents []byte
	var expectedDigest string
	if configPath != "" {
		_, contents, next, digest, err := loadValidatedConfigSource(configPath)
		if err != nil {
			return fmt.Errorf("validate reload configuration: %w", err)
		}
		current, err := config.LoadFile(manager.Layout.Config)
		if err != nil {
			return fmt.Errorf("load managed configuration before reload: %w", err)
		}
		if err := app.PreflightReload(ctx, current, next); err != nil {
			return fmt.Errorf("validate configuration for live reload: %w", err)
		}
		configContents = contents
		expectedDigest = digest
	}

	state, err := system.ReadState(manager.Layout.State)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return serviceReloadNotRunningError("")
		}
		return fmt.Errorf("read service state before reload: %w", err)
	}
	if state.StatusSocket == "" {
		return errors.New("service state has no runtime status socket")
	}
	before, err := runtimestatus.Query(ctx, state.StatusSocket)
	if err != nil {
		return fmt.Errorf("query runtime before reload: %w", err)
	}

	var configUpdate *launchservice.ConfigUpdate
	if configContents != nil {
		configUpdate, err = manager.BeginConfigUpdate(configContents)
		if err != nil {
			return fmt.Errorf("synchronize managed configuration: %w", err)
		}
	}

	after, reloadErr := requestServiceReload(ctx, manager, state.StatusSocket, before, options.timeout)
	if reloadErr == nil && expectedDigest != "" && after.ConfigDigest != expectedDigest {
		reloadErr = fmt.Errorf("runtime acknowledged config digest %q, want %q", after.ConfigDigest, expectedDigest)
	}
	if reloadErr != nil {
		if configUpdate == nil {
			return reloadErr
		}
		rollbackErr := rollbackServiceReloadConfig(manager, configUpdate, state.StatusSocket, options.timeout)
		if rollbackErr != nil {
			return errors.Join(fmt.Errorf("apply synchronized configuration: %w", reloadErr), rollbackErr)
		}
		return fmt.Errorf("apply synchronized configuration (managed configuration rolled back): %w", reloadErr)
	}
	if configUpdate != nil {
		if err := configUpdate.Commit(); err != nil {
			return fmt.Errorf("configuration reloaded but managed configuration commit cleanup failed: %w", err)
		}
		fmt.Printf("managed configuration synchronized: %s\n", manager.Layout.Config)
	}
	fmt.Printf("tun-proxy service reloaded config=%s successes=%d failures=%d\n", after.ConfigDigest, after.Reload.Successes, after.Reload.Failures)
	return nil
}

func resolveServiceReloadConfigPath(options serviceReloadOptions) (string, error) {
	if options.useUserConfig && options.configPath != "" {
		return "", errors.New("service reload -user-config and -config cannot be used together")
	}
	if options.useUserConfig {
		return defaultUserConfigPath(), nil
	}
	return options.configPath, nil
}

func requestServiceReload(
	ctx context.Context,
	manager *launchservice.Manager,
	socket string,
	before runtimestatus.Snapshot,
	timeout time.Duration,
) (runtimestatus.Snapshot, error) {
	if err := manager.Reload(ctx); err != nil {
		return runtimestatus.Snapshot{}, err
	}
	return waitForServiceReload(ctx, socket, before, timeout)
}

func rollbackServiceReloadConfig(
	manager *launchservice.Manager,
	update *launchservice.ConfigUpdate,
	socket string,
	timeout time.Duration,
) error {
	if err := update.Rollback(); err != nil {
		return fmt.Errorf("roll back managed configuration: %w", err)
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), timeout+25*time.Second)
	defer cancel()
	before, err := runtimestatus.Query(recoveryCtx, socket)
	if err != nil {
		return fmt.Errorf("query runtime before restoring rolled-back configuration: %w", err)
	}
	if _, err := requestServiceReload(recoveryCtx, manager, socket, before, timeout); err != nil {
		return fmt.Errorf("restore rolled-back configuration in runtime: %w", err)
	}
	return nil
}

func validateServiceReloadStatus(status launchservice.Status) error {
	if !status.Installed {
		return fmt.Errorf("tun-proxy service is not installed; run %q first", launchservice.InstallCommand)
	}
	if status.Runtime.Running && status.Runtime.Phase == "running" {
		return nil
	}
	return serviceReloadNotRunningError(status.Runtime.Phase)
}

func serviceReloadNotRunningError(phase string) error {
	return fmt.Errorf("tun-proxy service is not running (phase=%q); run %q first", phase, launchservice.StartCommand)
}

func waitForServiceReload(ctx context.Context, socket string, before runtimestatus.Snapshot, timeout time.Duration) (runtimestatus.Snapshot, error) {
	return waitForServiceReloadWithQuery(ctx, socket, before, timeout, runtimestatus.Query)
}

func waitForServiceReloadWithQuery(
	ctx context.Context,
	socket string,
	before runtimestatus.Snapshot,
	timeout time.Duration,
	query func(context.Context, string) (runtimestatus.Snapshot, error),
) (runtimestatus.Snapshot, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		after, err := query(ctx, socket)
		if err == nil {
			if after.Reload.Failures > before.Reload.Failures {
				message := after.Reload.LastError
				if message == "" {
					message = "runtime rejected the configuration reload"
				}
				return after, errors.New(message)
			}
			if after.Reload.Successes > before.Reload.Successes {
				return after, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return runtimestatus.Snapshot{}, ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return runtimestatus.Snapshot{}, fmt.Errorf("reload was not confirmed within %s: %w", timeout, lastErr)
			}
			return runtimestatus.Snapshot{}, fmt.Errorf("reload was not confirmed within %s", timeout)
		case <-ticker.C:
		}
	}
}

type serviceInstallOptions struct {
	configPath  string
	binaryPath  string
	start       bool
	startAtBoot bool
}

func newServiceInstallFlagSet(output io.Writer, options *serviceInstallOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceInstallOptions{}
	}
	flags := newCommandFlagSet("service install", output)
	flags.StringVar(&options.configPath, "config", defaultUserConfigPath(), "configuration `PATH` to install")
	flags.StringVar(&options.binaryPath, "binary", "", "tun-proxy binary `PATH` to install (default: current executable)")
	flags.BoolVar(&options.start, "start", true, "start after installation")
	flags.BoolVar(&options.startAtBoot, "start-at-boot", false, "start automatically at system boot")
	return flags
}

const serviceLogsHintCommand = "sudo tun-proxy service logs"

func serviceInstallCommand(ctx context.Context, manager *launchservice.Manager, args []string) (resultErr error) {
	defer func() {
		resultErr = withServiceLogsHint(resultErr)
	}()

	options := serviceInstallOptions{}
	flags := newServiceInstallFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service install received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	configSource, err := validateConfigSource(options.configPath)
	if err != nil {
		return err
	}
	binarySource, err := serviceBinarySource(options.binaryPath)
	if err != nil {
		return err
	}
	logCheckpoints := checkpointManagedLogs(manager.Layout)
	if err := manager.Install(ctx, binarySource, configSource, options.start, options.startAtBoot); err != nil {
		return withServiceInstallLogDiagnostics(err, manager.Layout, logCheckpoints)
	}
	if options.start {
		fmt.Println("tun-proxy service installed and started")
	} else {
		fmt.Println("tun-proxy service installed")
	}
	return nil
}

func withServiceLogsHint(err error) error {
	if err == nil || errors.Is(err, flag.ErrHelp) || strings.Contains(err.Error(), serviceLogsHintCommand) {
		return err
	}
	return fmt.Errorf("%w\nto inspect service logs, run: %s", err, serviceLogsHintCommand)
}

type serviceStatusOptions struct{ jsonOutput bool }

func newServiceStatusFlagSet(output io.Writer, options *serviceStatusOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceStatusOptions{}
	}
	flags := newCommandFlagSet("service status", output)
	flags.BoolVar(&options.jsonOutput, "json", false, "print service status as JSON")
	return flags
}

func serviceStatusCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	options := serviceStatusOptions{}
	flags := newServiceStatusFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service status received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if options.jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	}
	fmt.Printf("installed=%t loaded=%t running=%t pid=%d phase=%s\n",
		status.Installed, status.Loaded, status.Runtime.Running, status.Runtime.PID, status.Runtime.Phase)
	return nil
}

type serviceUpgradeOptions struct {
	binaryPath       string
	configPath       string
	startAtBootValue string
}

func newServiceUpgradeFlagSet(output io.Writer, options *serviceUpgradeOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceUpgradeOptions{}
	}
	flags := newCommandFlagSet("service upgrade", output)
	flags.StringVar(&options.binaryPath, "binary", "", "replacement binary `PATH` (default: current executable)")
	flags.StringVar(&options.configPath, "config", "", "optional replacement configuration `PATH`")
	flags.StringVar(&options.startAtBootValue, "start-at-boot", "", "optional boot startup `BOOL` (true or false; default: preserve current)")
	return flags
}

type serviceUpgrader interface {
	Upgrade(context.Context, string, string, *bool) (launchservice.UpgradeResult, error)
}

func serviceUpgradeCommand(ctx context.Context, manager serviceUpgrader, args []string) error {
	options := serviceUpgradeOptions{}
	flags := newServiceUpgradeFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service upgrade received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	binarySource, err := serviceBinarySource(options.binaryPath)
	if err != nil {
		return err
	}
	configSource := ""
	if options.configPath != "" {
		configSource, err = validateConfigSource(options.configPath)
		if err != nil {
			return err
		}
	}
	var startAtBoot *bool
	if options.startAtBootValue != "" {
		value, err := strconv.ParseBool(options.startAtBootValue)
		if err != nil {
			return fmt.Errorf("service upgrade start-at-boot must be true or false, got %q", options.startAtBootValue)
		}
		startAtBoot = &value
	}
	result, err := manager.Upgrade(ctx, binarySource, configSource, startAtBoot)
	if err != nil {
		return withServiceLogsHint(err)
	}
	fmt.Println(serviceUpgradeSuccessMessage(result))
	return nil
}

func serviceUpgradeSuccessMessage(result launchservice.UpgradeResult) string {
	if result.Restarted {
		return "tun-proxy service upgraded and restarted"
	}
	return "tun-proxy service upgraded; service remains stopped (startup not verified)"
}

type serviceUninstallOptions struct{ purge bool }

func newServiceUninstallFlagSet(output io.Writer, options *serviceUninstallOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceUninstallOptions{}
	}
	flags := newCommandFlagSet("service uninstall", output)
	flags.BoolVar(&options.purge, "purge", false, "also remove installed config, mappings, and logs")
	return flags
}

func serviceUninstallCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	options := serviceUninstallOptions{}
	flags := newServiceUninstallFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service uninstall received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := manager.Uninstall(ctx, options.purge); err != nil {
		return err
	}
	if options.purge {
		fmt.Println("tun-proxy service uninstalled and managed data purged")
	} else {
		fmt.Println("tun-proxy service uninstalled; config, mappings, and logs preserved")
	}
	return nil
}

func serviceRunCommand(args []string) error {
	flags := flag.NewFlagSet("_service-run", flag.ContinueOnError)
	path := flags.String("config", "", "installed service configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("_service-run received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	layout := launchservice.DefaultLayout()
	if *path != layout.Config {
		return fmt.Errorf("managed service requires -config %q", layout.Config)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, unix.SIGHUP)
	defer signal.Stop(hup)
	reload := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				select {
				case reload <- struct{}{}:
				default:
				}
			}
		}
	}()
	return app.RunServiceSupervisor(ctx, *path, layout, app.ServiceSupervisorOptions{
		Reload: reload,
		Ready: func(tunName string) {
			writeManagedServiceLog(os.Stdout, time.Now(), fmt.Sprintf("tun-proxy service running tun=%s", tunName))
		},
		Event: func(level, message string) {
			writeManagedServiceLog(os.Stdout, time.Now(), fmt.Sprintf("%s %s", strings.ToUpper(level), message))
		},
	})
}

const managedServiceTimestampLayout = "2006-01-02T15:04:05.000-07:00"

// writeManagedServiceLog prefixes every physical log line with a local RFC 3339
// timestamp. Building the complete payload before writing also keeps multi-line
// errors from interleaving with concurrent supervisor output.
func writeManagedServiceLog(output io.Writer, now time.Time, message string) {
	timestamp := now.Local().Format(managedServiceTimestampLayout)
	message = strings.TrimSuffix(message, "\n")
	lines := strings.Split(message, "\n")
	var payload strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&payload, "%s %s\n", timestamp, line)
	}
	_, _ = io.WriteString(output, payload.String())
}

func serviceWorkerCommand(args []string) error {
	if err := validateServiceWorkerInvocation(
		args,
		func() (privsep.Identity, error) {
			return privsep.ResolveProductionIdentity(privsep.SystemDirectory{})
		},
		privsep.ValidateCurrentIdentity,
	); err != nil {
		return err
	}
	resources, err := privsep.OpenInheritedResources()
	if err != nil {
		return err
	}
	defer resources.Close() //nolint:errcheck // Best-effort cleanup.
	worker, err := app.NewServiceWorker(resources)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	return privsep.ServeWorker(ctx, resources.Control, worker)
}

func validateServiceWorkerInvocation(
	args []string,
	resolveIdentity func() (privsep.Identity, error),
	validateIdentity func(privsep.Identity) error,
) error {
	if len(args) != 0 {
		return fmt.Errorf("_service-worker does not accept arguments: %s", strings.Join(args, " "))
	}
	if resolveIdentity == nil || validateIdentity == nil {
		return errors.New("service worker identity validation is required")
	}
	identity, err := resolveIdentity()
	if err != nil {
		return err
	}
	if err := validateIdentity(identity); err != nil {
		return err
	}
	return nil
}

func newServiceManager() *launchservice.Manager {
	return newServiceManagerForLayout(launchservice.DefaultLayout())
}

func newServiceManagerForLayout(layout launchservice.Layout) *launchservice.Manager {
	manager := launchservice.NewManager(layout)
	manager.Recover = func(ctx context.Context) error {
		owners := []uint32{0}
		if identity, err := launchservice.ResolveWorkerIdentity(ctx); err == nil {
			owners = append(owners, identity.UID)
		}
		return app.CleanupWithStatusOwners(ctx, layout.State, layout.Lock, owners...)
	}
	return manager
}

func validateServiceRuntime(runtime *config.Config) error {
	return launchservice.ValidateManagedConfig(runtime, launchservice.DefaultLayout())
}

func validateConfigSource(path string) (string, error) {
	absolute, _, _, _, err := loadValidatedConfigSource(path)
	return absolute, err
}

func loadValidatedConfigSource(path string) (string, []byte, *config.Config, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("resolve source path %q: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("inspect source %q: %w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, nil, "", fmt.Errorf("source %q must be a regular file, not a symlink", absolute)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("open source %q: %w", absolute, err)
	}
	defer file.Close() //nolint:errcheck // Best-effort read-only source cleanup.
	opened, err := file.Stat()
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("inspect opened source %q: %w", absolute, err)
	}
	if !os.SameFile(info, opened) {
		return "", nil, nil, "", fmt.Errorf("source %q changed while opening", absolute)
	}
	contents, err := io.ReadAll(io.LimitReader(file, privsep.MaxConfigSize+1))
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("read source %q: %w", absolute, err)
	}
	if len(contents) > privsep.MaxConfigSize {
		return "", nil, nil, "", fmt.Errorf("source %q exceeds %d bytes", absolute, privsep.MaxConfigSize)
	}
	runtime, digest, err := config.LoadBytesWithDigest(contents)
	if err != nil {
		return "", nil, nil, "", err
	}
	if err := validateServiceRuntime(runtime); err != nil {
		return "", nil, nil, "", err
	}
	return absolute, contents, runtime, digest, nil
}

func serviceBinarySource(path string) (string, error) {
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate current executable: %w", err)
		}
	}
	return absoluteRegularSource(path)
}

func absoluteRegularSource(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve source path %q: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect source %q: %w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %q must be a regular file, not a symlink", absolute)
	}
	return absolute, nil
}

func printServiceUsage() {
	fprintUsage(os.Stderr, []string{"service"})
}
