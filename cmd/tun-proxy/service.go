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
		if hasOnlyHelpArgument(args[1:]) {
			return helpCommand([]string{"service", "start"})
		}
		if len(args) != 1 {
			return errors.New("service start does not accept arguments")
		}
		if err := manager.Start(ctx); err != nil {
			return err
		}
		fmt.Println("tun-proxy service started")
		return nil
	case "stop":
		if hasOnlyHelpArgument(args[1:]) {
			return helpCommand([]string{"service", "stop"})
		}
		if len(args) != 1 {
			return errors.New("service stop does not accept arguments")
		}
		if err := manager.Stop(ctx); err != nil {
			return err
		}
		fmt.Println("tun-proxy service stopped")
		return nil
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

func serviceReloadCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	flags := flag.NewFlagSet("service reload", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fprintUsage(flags.Output(), []string{"service", "reload"}) }
	timeout := flags.Duration("timeout", 15*time.Second, "wait for runtime confirmation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service reload received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *timeout <= 0 {
		return errors.New("service reload timeout must be positive")
	}
	state, err := system.ReadState(manager.Layout.State)
	if err != nil {
		return fmt.Errorf("read service state before reload: %w", err)
	}
	if state.StatusSocket == "" {
		return errors.New("service state has no runtime status socket")
	}
	before, err := runtimestatus.Query(ctx, state.StatusSocket)
	if err != nil {
		return fmt.Errorf("query runtime before reload: %w", err)
	}
	if err := manager.Reload(ctx); err != nil {
		return err
	}
	after, err := waitForServiceReload(ctx, state.StatusSocket, before, *timeout)
	if err != nil {
		return err
	}
	fmt.Printf("tun-proxy service reloaded config=%s successes=%d failures=%d\n", after.ConfigDigest, after.Reload.Successes, after.Reload.Failures)
	return nil
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

func serviceInstallCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	configPath := flags.String("config", defaultUserConfigPath(), "configuration to install")
	binaryPath := flags.String("binary", "", "tun-proxy binary to install (default: current executable)")
	start := flags.Bool("start", true, "start the service after installation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service install received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	configSource, err := validateConfigSource(*configPath)
	if err != nil {
		return err
	}
	binarySource, err := serviceBinarySource(*binaryPath)
	if err != nil {
		return err
	}
	if err := manager.Install(ctx, binarySource, configSource, *start); err != nil {
		return err
	}
	if *start {
		fmt.Println("tun-proxy service installed and started")
	} else {
		fmt.Println("tun-proxy service installed")
	}
	return nil
}

func serviceStatusCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	flags := flag.NewFlagSet("service status", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "print service status as JSON")
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
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	}
	fmt.Printf("installed=%t loaded=%t running=%t pid=%d phase=%s\n",
		status.Installed, status.Loaded, status.Runtime.Running, status.Runtime.PID, status.Runtime.Phase)
	return nil
}

func serviceUpgradeCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	flags := flag.NewFlagSet("service upgrade", flag.ContinueOnError)
	binaryPath := flags.String("binary", "", "replacement binary (default: current executable)")
	configPath := flags.String("config", "", "optional replacement configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service upgrade received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	binarySource, err := serviceBinarySource(*binaryPath)
	if err != nil {
		return err
	}
	configSource := ""
	if *configPath != "" {
		configSource, err = validateConfigSource(*configPath)
		if err != nil {
			return err
		}
	}
	if err := manager.Upgrade(ctx, binarySource, configSource); err != nil {
		return err
	}
	fmt.Println("tun-proxy service upgraded")
	return nil
}

func serviceUninstallCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	flags := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	purge := flags.Bool("purge", false, "also remove installed config, mappings, and logs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service uninstall received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := manager.Uninstall(ctx, *purge); err != nil {
		return err
	}
	if *purge {
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
	absolute, err := absoluteRegularSource(path)
	if err != nil {
		return "", err
	}
	runtime, err := config.LoadFile(absolute)
	if err != nil {
		return "", err
	}
	if err := validateServiceRuntime(runtime); err != nil {
		return "", err
	}
	return absolute, nil
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
