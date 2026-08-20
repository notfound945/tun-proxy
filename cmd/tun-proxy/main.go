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
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/app"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/interfaceinfo"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	"github.com/hailinpan/tun-proxy/internal/system"
	"golang.org/x/sys/unix"
)

const cleanupCommandTimeout = 30 * time.Second

var (
	version   = "local"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if isManagedServiceProcess(os.Args[1:]) {
			writeManagedServiceLog(os.Stderr, time.Now(), "ERROR "+err.Error())
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func isManagedServiceProcess(args []string) bool {
	return len(args) > 0 && (args[0] == "_service-run" || args[0] == "_service-worker")
}

func run(args []string) error {
	return runWithVersionOutput(args, os.Stdout)
}

func runWithVersionOutput(args []string, versionOutput io.Writer) error {
	if len(args) == 0 {
		printUsage()
		return fmt.Errorf("a command is required")
	}

	switch args[0] {
	case "interfaces":
		if len(args) != 1 {
			return errors.New("interfaces does not accept arguments")
		}
		return printInterfaces()
	case "check":
		return checkCommand(args[1:])
	case "config":
		return configCommand(args[1:])
	case "explain":
		return explainCommand(args[1:])
	case "diagnose":
		return diagnoseCommand(args[1:])
	case "run":
		return runCommand(args[1:])
	case "status":
		return statusCommand(args[1:])
	case "cleanup":
		return cleanupCommand(args[1:])
	case "service":
		return serviceCommand(args[1:])
	case "_service-run":
		return serviceRunCommand(args[1:])
	case "_service-worker":
		return serviceWorkerCommand(args[1:])
	case "version", "-version", "--version":
		if len(args) != 1 {
			return fmt.Errorf("%s does not accept arguments", args[0])
		}
		_, err := fmt.Fprintf(versionOutput, "tun-proxy %s (commit %s, built %s)\n", version, commit, buildTime)
		return err
	case "help":
		return helpCommand(args[1:])
	case "-h", "--help":
		return helpCommand(nil)
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type checkOptions struct {
	configPath string
	service    bool
}

func newCheckFlagSet(output io.Writer, options *checkOptions) *flag.FlagSet {
	if options == nil {
		options = &checkOptions{}
	}
	flags := newCommandFlagSet("check", output)
	flags.StringVar(&options.configPath, "config", defaultUserConfigPath(), "`PATH` to YAML configuration")
	flags.BoolVar(&options.service, "service", false, "validate the installed split-privilege service layout")
	return flags
}

func checkCommand(args []string) error {
	options := checkOptions{}
	flags := newCheckFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("check received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	runtime, err := config.LoadFile(options.configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var preflightErr error
	if options.service {
		layout := launchservice.DefaultLayout()
		if err := launchservice.ValidateManagedConfig(runtime, layout); err != nil {
			return fmt.Errorf("managed service configuration: %w", err)
		}
		identity, err := launchservice.ResolveWorkerIdentity(ctx)
		if err != nil {
			return fmt.Errorf("resolve managed service worker: %w", err)
		}
		if err := launchservice.ValidateWorkerStorage(layout, identity); err != nil {
			return fmt.Errorf("validate managed worker storage: %w", err)
		}
		preflightErr = app.PreflightManaged(ctx, runtime, identity.UID)
	} else {
		preflightErr = app.Preflight(ctx, runtime)
	}
	if preflightErr != nil {
		return fmt.Errorf("preflight failed:\n%w", preflightErr)
	}
	fmt.Println(app.Summary(runtime))
	if enabled, reason := app.IPv6DataPathAvailable(context.Background(), runtime); runtime.FakeIPv6 != nil && !enabled {
		fmt.Printf("WARN IPv6 data path disabled: %s; Fake AAAA will return NODATA\n", reason)
	}
	return nil
}

type runOptions struct{ configPath string }

func newRunFlagSet(output io.Writer, options *runOptions) *flag.FlagSet {
	if options == nil {
		options = &runOptions{}
	}
	flags := newCommandFlagSet("run", output)
	flags.StringVar(&options.configPath, "config", defaultUserConfigPath(), "`PATH` to YAML configuration")
	return flags
}

func runCommand(args []string) error {
	options := runOptions{}
	flags := newRunFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("run received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return runConfigured(options.configPath, false)
}

func runConfigured(path string, managed bool) error {
	load := func() (*config.Config, string, error) {
		runtime, digest, err := config.LoadFileWithDigest(path)
		if err != nil {
			return nil, "", err
		}
		if managed {
			if err := validateServiceRuntime(runtime); err != nil {
				return nil, "", err
			}
		}
		return runtime, digest, nil
	}
	runtime, digest, err := load()
	if err != nil {
		return err
	}
	if err := app.Preflight(context.Background(), runtime); err != nil {
		return fmt.Errorf("preflight failed:\n%w", err)
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
	stats, err := app.Run(ctx, runtime, digest, app.RunOptions{
		Ready: func(tunName string) {
			if managed {
				fmt.Printf("tun-proxy service running tun=%s fake_ip=%s dns=%s\n", tunName, runtime.FakeIP.Prefix, runtime.DNS.Listen)
				return
			}
			fmt.Printf("tun-proxy running tun=%s fake_ip=%s dns=%s; press Ctrl-C to stop\n", tunName, runtime.FakeIP.Prefix, runtime.DNS.Listen)
		},
		Reload:     reload,
		LoadConfig: load,
		Event: func(level, message string) {
			fmt.Printf("%s %s\n", strings.ToUpper(level), message)
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("tun-proxy stopped cleanly tcp_flows=%d udp_sessions=%d dns_queries=%d tun_rx_packets=%d tun_tx_packets=%d\n",
		stats.TCP.TotalFlows, stats.UDP.TotalSessions, stats.DNS.Queries, stats.TUN.ReceivedPackets, stats.TUN.TransmittedPackets)
	return nil
}

type statusOptions struct {
	statePath  string
	jsonOutput bool
}

func newStatusFlagSet(output io.Writer, options *statusOptions) *flag.FlagSet {
	if options == nil {
		options = &statusOptions{}
	}
	flags := newCommandFlagSet("status", output)
	flags.StringVar(&options.statePath, "state", "/var/run/tun-proxy/state.json", "runtime state `PATH`")
	flags.BoolVar(&options.jsonOutput, "json", false, "print the complete runtime snapshot as JSON")
	return flags
}

func statusCommand(args []string) error {
	options := statusOptions{}
	flags := newStatusFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("status received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	state, err := system.ReadState(options.statePath)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("tun-proxy is stopped (no state file)")
		return nil
	}
	if err != nil {
		return err
	}
	processErr := unix.Kill(state.PID, 0)
	alive := processErr == nil || errors.Is(processErr, unix.EPERM)
	if !alive || state.StatusSocket == "" {
		if options.jsonOutput {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(state)
		}
		fmt.Printf("phase=%s pid=%d alive=%t started=%s tun=%s\n", state.Phase, state.PID, alive, state.StartedAt.Format("2006-01-02T15:04:05Z07:00"), state.TUNName)
		return nil
	}
	snapshot, err := runtimestatus.Query(context.Background(), state.StatusSocket)
	if err != nil {
		return err
	}
	if options.jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(snapshot)
	}
	fmt.Printf("phase=%s pid=%d alive=%t started=%s tun=%s\n", state.Phase, state.PID, alive, state.StartedAt.Format("2006-01-02T15:04:05Z07:00"), state.TUNName)
	fmt.Printf("uptime=%s config=%s reloads=%d reload_failures=%d goroutines=%d open_fds=%d heap=%d\n",
		time.Since(snapshot.StartedAt).Round(time.Second), snapshot.ConfigDigest,
		snapshot.Reload.Successes, snapshot.Reload.Failures, snapshot.Resources.Goroutines,
		snapshot.Resources.OpenFDs, snapshot.Resources.HeapAlloc)
	fmt.Printf("tcp_active=%d tcp_total=%d udp_active=%d udp_total=%d dns_queries=%d fake_ip_used=%d tun_rx_packets=%d tun_tx_packets=%d\n",
		snapshot.TCP.ActiveFlows, snapshot.TCP.TotalFlows, snapshot.UDP.ActiveSessions,
		snapshot.UDP.TotalSessions, snapshot.DNS.Queries, snapshot.FakeIP.Used,
		snapshot.TUN.ReceivedPackets, snapshot.TUN.TransmittedPackets)
	if snapshot.IPv6.Configured {
		fmt.Printf("fake_ipv6_configured=true fake_ipv6_used=%d fake_ipv6_limit=%d aaaa_enabled=%t",
			snapshot.FakeIPv6.Used, snapshot.Limits.FakeIPv6Mappings, snapshot.IPv6.Enabled)
		if snapshot.IPv6.FallbackReason != "" {
			fmt.Printf(" fallback_reason=%q", snapshot.IPv6.FallbackReason)
		}
		fmt.Println()
	}
	return nil
}

type cleanupOptions struct {
	statePath string
	lockPath  string
	timeout   time.Duration
}

func newCleanupFlagSet(output io.Writer, options *cleanupOptions) *flag.FlagSet {
	if options == nil {
		options = &cleanupOptions{}
	}
	flags := newCommandFlagSet("cleanup", output)
	flags.StringVar(&options.statePath, "state", "/var/run/tun-proxy/state.json", "runtime state `PATH`")
	flags.StringVar(&options.lockPath, "lock", "/var/run/tun-proxy/tun-proxy.lock", "fallback stale lock `PATH`")
	flags.DurationVar(&options.timeout, "timeout", cleanupCommandTimeout, "maximum cleanup `DURATION`")
	return flags
}

func cleanupCommand(args []string) error {
	options := cleanupOptions{}
	flags := newCleanupFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("cleanup received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if options.timeout <= 0 {
		return errors.New("cleanup timeout must be positive")
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	cleanupCtx, cancel := context.WithTimeout(signalCtx, options.timeout)
	defer cancel()
	if err := app.Cleanup(cleanupCtx, options.statePath, options.lockPath); err != nil {
		return err
	}
	fmt.Println("cleanup complete")
	return nil
}

func printInterfaces() error {
	interfaces, err := interfaceinfo.List()
	if err != nil {
		return err
	}

	fmt.Printf("%-6s %-5s %-7s %-8s %-5s %s\n", "NAME", "INDEX", "UP", "RUNNING", "MTU", "ADDRESSES")
	for _, iface := range interfaces {
		fmt.Printf("%-6s %-5d %-7t %-8t %-5d %s\n",
			iface.Name,
			iface.Index,
			iface.Up(),
			iface.Running(),
			iface.MTU,
			strings.Join(iface.Addresses, ","),
		)
	}
	return nil
}

func printUsage() {
	fprintUsage(os.Stderr, nil)
}
