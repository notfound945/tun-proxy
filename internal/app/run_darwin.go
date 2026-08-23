//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/daemon"
	"github.com/hailinpan/tun-proxy/internal/fakedns"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	"github.com/hailinpan/tun-proxy/internal/session"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	"github.com/hailinpan/tun-proxy/internal/system"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
)

const (
	runtimeDNSQueryTimeout = 5 * time.Second
	runtimeRelayGrace      = 30 * time.Second
	runtimeShutdownTimeout = 10 * time.Second
	runtimeNetworkPoll     = 5 * time.Second
)

type RunStats struct {
	TUN      internaltun.Stats
	Netstack netstack.Stats
	TCP      session.Stats
	UDP      session.UDPStats
	DNS      fakedns.Stats
	FakeIP   fakeip.Stats
	FakeIPv6 fakeip.Stats
}

type RunOptions struct {
	Ready      func(tunName string)
	Reload     <-chan struct{}
	LoadConfig func() (*config.Config, string, error)
	Event      func(level, message string)
}

// Run starts the TCP MVP and owns every system mutation until it has been
// rolled back. The ready callback runs only after TUN, route, data plane, Fake
// DNS, and optional system DNS changes are all live.
func Run(ctx context.Context, runtime *config.Config, configDigest string, options RunOptions) (stats RunStats, resultErr error) {
	if runtime == nil {
		return stats, errors.New("runtime config is required")
	}
	if configDigest == "" {
		return stats, errors.New("config digest is required")
	}
	if err := system.RequireRoot(); err != nil {
		return stats, err
	}
	configureLogging(runtime.Log)
	runner := system.NativeCommandRunner{}
	interfaceServers, err := discoverInterfaceDNS(ctx, runtime, runner)
	if err != nil {
		emitRunEvent(options, "warn", "discover interface DHCP DNS: "+err.Error()+"; configured DNS will be used where discovery failed")
	}
	effectiveRuntime := runtimeWithInterfaceDNS(runtime, interfaceServers)
	for _, message := range effectiveDNSMessages(runtime, interfaceServers) {
		emitRunEvent(options, "info", message)
	}
	ipv6Enabled, ipv6FallbackReason := IPv6DataPathAvailable(ctx, runtime)
	if runtime.FakeIPv6 != nil && !ipv6Enabled {
		emitRunEvent(options, "warn", "IPv6 data path disabled: "+ipv6FallbackReason+"; Fake AAAA will return NODATA until restart")
	}
	defaultRoutes, err := planDefaultRouteCapture(ctx, effectiveRuntime, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped)
	if err != nil {
		return stats, err
	}

	// Construct and validate the entire in-process graph before touching the
	// host network.
	fakePool, err := fakeip.New(runtime.FakeIP.Prefix, runtime.FakeIP.MappingTTL, runtime.FakeIP.MaxMappings, 10)
	if err != nil {
		return stats, err
	}
	persistence, quarantined, err := fakeip.OpenPersistence(runtime.FakeIP.PersistenceFile, fakePool, runtime.FakeIP.DNSTTL)
	if err != nil {
		return stats, fmt.Errorf("open Fake IP persistence: %w", err)
	}
	if quarantined != "" {
		slog.Warn("quarantined invalid Fake IP persistence", "path", quarantined)
	}
	var fakeIPv6Pool *fakeip.Pool
	var fakeIPv6Persistence *fakeip.Persistence
	if runtime.FakeIPv6 != nil {
		fakeIPv6Pool, err = fakeip.New(runtime.FakeIPv6.Prefix, runtime.FakeIP.MappingTTL, runtime.FakeIPv6.MaxMappings, 10)
		if err != nil {
			return stats, fmt.Errorf("create Fake IPv6 pool: %w", err)
		}
		fakeIPv6Persistence, quarantined, err = fakeip.OpenPersistence(
			runtime.FakeIPv6.PersistenceFile, fakeIPv6Pool, runtime.FakeIP.DNSTTL,
		)
		if err != nil {
			return stats, fmt.Errorf("open Fake IPv6 persistence: %w", err)
		}
		if quarantined != "" {
			slog.Warn("quarantined invalid Fake IPv6 persistence", "path", quarantined)
		}
	}
	activeFakeIPv6Pool := fakeIPv6Pool
	if !ipv6Enabled {
		activeFakeIPv6Pool = nil
	}
	dataPlane, err := newDataPlane(fakePool, activeFakeIPv6Pool, effectiveRuntime)
	if err != nil {
		return stats, err
	}
	ipStack, err := netstack.New(netstack.Config{
		MTU: runtime.TUN.MTU, PacketQueue: runtime.TUN.PacketQueue, MaxTCPFlows: runtime.Sessions.MaxTCPFlows,
	}, dataPlane.handleTCP, dataPlane.handleUDP)
	if err != nil {
		return stats, err
	}
	stackOpen := true
	closeStack := func() error {
		if !stackOpen {
			return nil
		}
		stackOpen = false
		return ipStack.Close()
	}

	defaultOutbound := effectiveRuntime.Outbounds[effectiveRuntime.DNS.DefaultOutbound]
	forwardResolver, err := resolver.NewClient(defaultOutbound.Interface, defaultOutbound.DNS, runtimeDNSQueryTimeout, runtime.DNS.MaxConcurrent)
	if err != nil {
		_ = closeStack()
		return stats, fmt.Errorf("build Fake DNS forwarding resolver: %w", err)
	}
	dnsServer, err := fakedns.NewDualStack(fakedns.Config{
		Listen:        runtime.DNS.Listen,
		UDP:           runtime.DNS.UDP,
		TCP:           runtime.DNS.TCP,
		TTL:           runtime.FakeIP.DNSTTL,
		QueryTimeout:  runtimeDNSQueryTimeout,
		MaxConcurrent: runtime.DNS.MaxConcurrent,
		ShouldFake:    dataPlane.shouldFake,
	}, fakePool, activeFakeIPv6Pool, runtime.FakeIP.Exclude, forwardResolver)
	if err != nil {
		_ = closeStack()
		return stats, err
	}

	if _, err := os.Lstat(runtime.System.StateFile); err == nil {
		_ = closeStack()
		return stats, fmt.Errorf("recovery state %q already exists; run cleanup before starting", runtime.System.StateFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = closeStack()
		return stats, fmt.Errorf("inspect recovery state %q: %w", runtime.System.StateFile, err)
	}

	lock, err := daemon.Acquire(runtime.System.LockFile)
	if err != nil {
		_ = closeStack()
		return stats, err
	}
	state := system.NewState(configDigest)
	state.LockFile = runtime.System.LockFile
	monitor := newRunMonitor(state.StartedAt, configDigest, runtime, ipv6Enabled, ipv6FallbackReason)
	stateExists := false
	var device *internaltun.Device
	var packetPump *internaltun.Pump
	var runningDNS *fakedns.Running
	var statusServer *runtimestatus.Server
	dataCtx, cancelData := context.WithCancel(context.Background())
	defer cancelData()
	pumpDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	pumpStarted := false
	outputStarted := false
	pumpConsumed := false
	outputConsumed := false

	finish := func(primary error) (RunStats, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
		defer cancel()
		var cleanupErrors []error
		if statusServer != nil {
			if err := statusServer.Close(cleanupCtx); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("stop runtime status: %w", err))
			}
			statusServer = nil
			state.StatusSocket = ""
			if stateExists {
				if err := system.WriteState(runtime.System.StateFile, state); err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("persist status cleanup: %w", err))
				}
			}
		}
		if err := persistence.Flush(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("flush Fake IP persistence: %w", err))
		}
		if fakeIPv6Persistence != nil {
			if err := fakeIPv6Persistence.Flush(); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("flush Fake IPv6 persistence: %w", err))
			}
		}
		if stateExists {
			state.Phase = "stopping"
			if err := system.WriteState(runtime.System.StateFile, state); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("persist stopping state: %w", err))
			}
		}

		// Restore host DNS while Fake DNS is still listening.
		if len(state.DNS) != 0 {
			remaining, err := system.RestoreDNS(cleanupCtx, runner, state.DNS)
			state.DNS = remaining
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("restore system DNS: %w", err))
			}
			if stateExists {
				if err := system.WriteState(runtime.System.StateFile, state); err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("persist DNS rollback: %w", err))
				}
			}
		}
		if runningDNS != nil {
			if err := runningDNS.Close(cleanupCtx); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("stop Fake DNS: %w", err))
			}
			runningDNS = nil
		}

		// Stop new traffic before closing the TUN file descriptor.
		if stateExists && (state.Route != nil || len(state.Routes) != 0) {
			if err := removeRecordedRoutes(cleanupCtx, runner, runtime.System.StateFile, &state); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		cancelData()
		if device != nil {
			if err := device.Close(); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("close %s: %w", device.Name(), err))
			}
		}
		if pumpStarted && !pumpConsumed {
			select {
			case err := <-pumpDone:
				if err != nil {
					cleanupErrors = append(cleanupErrors, err)
				}
			case <-cleanupCtx.Done():
				cleanupErrors = append(cleanupErrors, errors.New("TUN packet pump did not stop before shutdown timeout"))
			}
		}
		if outputStarted && !outputConsumed {
			select {
			case err := <-outputDone:
				if err != nil {
					cleanupErrors = append(cleanupErrors, err)
				}
			case <-cleanupCtx.Done():
				cleanupErrors = append(cleanupErrors, errors.New("netstack output pump did not stop before shutdown timeout"))
			}
		}
		if err := closeStack(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		if err := lock.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("release process lock: %w", err))
		}
		cleanupErr := errors.Join(cleanupErrors...)
		if cleanupErr == nil && stateExists {
			if err := system.RemoveState(runtime.System.StateFile); err != nil {
				cleanupErr = err
			}
		}
		stats.Netstack = ipStack.Stats()
		stats.TCP, stats.UDP = dataPlane.stats()
		stats.DNS = dnsServer.Stats()
		stats.FakeIP = fakePool.Stats()
		if fakeIPv6Pool != nil {
			stats.FakeIPv6 = fakeIPv6Pool.Stats()
		}
		if packetPump != nil {
			stats.TUN = packetPump.Stats()
		}
		return stats, errors.Join(primary, cleanupErr)
	}

	if err := system.WriteState(runtime.System.StateFile, state); err != nil {
		_ = lock.Close()
		_ = closeStack()
		return stats, err
	}
	stateExists = true
	tunSettings := runtime.TUN
	if !ipv6Enabled {
		tunSettings.IPv6Address = netip.Addr{}
		tunSettings.IPv6Peer = netip.Addr{}
	}
	device, err = internaltun.Create(ctx, tunSettings, runner)
	if err != nil {
		return finish(err)
	}
	state.TUNName = device.Name()
	if err := system.WriteState(runtime.System.StateFile, state); err != nil {
		return finish(err)
	}

	route := system.RouteState{Prefix: runtime.FakeIP.Prefix.String(), Interface: device.Name()}
	state.Route = &route
	if err := system.WriteState(runtime.System.StateFile, state); err != nil {
		return finish(err)
	}
	if err := system.AddRoute(ctx, runner, route); err != nil {
		return finish(err)
	}
	if err := system.VerifyRoute(ctx, runner, route); err != nil {
		return finish(err)
	}
	if ipv6Enabled {
		ipv6Route := system.RouteState{Prefix: runtime.FakeIPv6.Prefix.String(), Interface: device.Name()}
		state.Routes = append(state.Routes, ipv6Route)
		if err := system.WriteState(runtime.System.StateFile, state); err != nil {
			return finish(err)
		}
		if err := system.AddRoute(ctx, runner, ipv6Route); err != nil {
			return finish(err)
		}
		if err := system.VerifyRoute(ctx, runner, ipv6Route); err != nil {
			return finish(err)
		}
	}
	for _, captureRoute := range defaultRoutes.routes(device.Name()) {
		state.Routes = append(state.Routes, captureRoute)
		if err := system.WriteState(runtime.System.StateFile, state); err != nil {
			return finish(err)
		}
		if err := system.AddRoute(ctx, runner, captureRoute); err != nil {
			return finish(err)
		}
		if err := system.VerifyRoute(ctx, runner, captureRoute); err != nil {
			return finish(err)
		}
	}
	if runtime.Capture.DefaultRoute {
		verifiedPlan, err := planDefaultRouteCaptureOwned(ctx, runtime, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped, defaultRoutes.Bypasses)
		if err != nil || !defaultRoutes.equal(verifiedPlan) {
			return finish(fmt.Errorf("prove loop-free egress after default-route capture: %w", errors.Join(err, errors.New("captured route plan changed during installation"))))
		}
	}

	packetBuffers, err := internaltun.NewBufferPool(internaltun.PacketOffset+runtime.TUN.MTU+256, runtime.TUN.BufferPool)
	if err != nil {
		return finish(err)
	}
	packetPump, err = internaltun.NewPump(device, packetBuffers, func(_ context.Context, packet []byte) error {
		return ipStack.InjectPacket(packet)
	})
	if err != nil {
		return finish(err)
	}
	pumpStarted = true
	go func() { pumpDone <- packetPump.Run(dataCtx) }()
	outputStarted = true
	go func() { outputDone <- pumpNetstackOutput(dataCtx, ipStack, packetPump) }()

	// Both UDP and TCP listeners are bound before any system resolver changes.
	runningDNS, err = dnsServer.Start()
	if err != nil {
		return finish(err)
	}
	select {
	case <-runningDNS.Done():
		if err := runningDNS.Err(); err != nil {
			return finish(err)
		}
		return finish(errors.New("Fake DNS stopped unexpectedly"))
	default:
	}
	if runtime.System.ManageDNS {
		snapshot, err := system.SnapshotDNS(ctx, runner, runtime.DNS.Listen.Addr())
		if err != nil {
			return finish(err)
		}
		planned, err := system.PlanDNS(snapshot, runtime.DNS.Listen.Addr())
		if err != nil {
			return finish(err)
		}
		state.DNS = planned
		state.Phase = "dns-planned"
		if err := system.WriteState(runtime.System.StateFile, state); err != nil {
			return finish(err)
		}
		if _, err := system.ApplyDNS(ctx, runner, planned, runtime.DNS.Listen.Addr()); err != nil {
			return finish(err)
		}
	}
	statusPath := runtime.System.StateFile + ".sock"
	statusServer, err = runtimestatus.StartWithOptions(statusPath, func(options runtimestatus.QueryOptions) runtimestatus.Snapshot {
		snapshot := monitor.snapshot()
		snapshot.Resources = runtimestatus.Resources()
		snapshot.Netstack = ipStack.Stats()
		snapshot.TCP, snapshot.UDP = dataPlane.stats()
		snapshot.DNS = dnsServer.Stats()
		snapshot.FakeIP = fakePool.Stats()
		if fakeIPv6Pool != nil {
			snapshot.FakeIPv6 = fakeIPv6Pool.Stats()
		}
		if options.IncludeFakeIPMappings {
			ipv4Mappings := fakePool.Snapshot().Mappings
			var ipv6Mappings []fakeip.Mapping
			if fakeIPv6Pool != nil {
				ipv6Mappings = fakeIPv6Pool.Snapshot().Mappings
			}
			snapshot.FakeIPMappings = runtimestatus.NewMappingSet(ipv4Mappings, ipv6Mappings)
		}
		if packetPump != nil {
			snapshot.TUN = packetPump.Stats()
		}
		return snapshot
	})
	if err != nil {
		return finish(err)
	}
	state.StatusSocket = statusPath
	state.Phase = "running"
	if err := system.WriteState(runtime.System.StateFile, state); err != nil {
		return finish(err)
	}
	select {
	case <-runningDNS.Done():
		if err := runningDNS.Err(); err != nil {
			return finish(err)
		}
		return finish(errors.New("Fake DNS stopped unexpectedly"))
	default:
	}
	if options.Ready != nil {
		options.Ready(device.Name())
	}
	networkTicker := time.NewTicker(runtimeNetworkPoll)
	defer networkTicker.Stop()
	networkState := newNetworkRefreshState(networkFingerprint(effectiveRuntime))

	var primary error
	for primary == nil {
		select {
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.Canceled) {
				primary = ctx.Err()
			} else {
				primary = errRunStopped
			}
		case <-runningDNS.Done():
			if err := runningDNS.Err(); err != nil {
				primary = err
			} else {
				primary = errors.New("Fake DNS stopped unexpectedly")
			}
		case <-options.Reload:
			next, nextEffective, nextInterfaceServers, err := reloadRuntime(
				ctx, runtime, &configDigest, &state, options, dataPlane, dnsServer,
				runner, defaultRoutes, ipv6Enabled,
			)
			monitor.reloadResult(time.Now().UTC(), configDigest, next, err)
			if err != nil {
				emitRunEvent(options, "warn", "configuration reload rejected: "+err.Error())
				continue
			}
			runtime = next
			effectiveRuntime = nextEffective
			interfaceServers = nextInterfaceServers
			for _, message := range effectiveDNSMessages(runtime, interfaceServers) {
				emitRunEvent(options, "info", message)
			}
			networkState.reset(networkFingerprint(effectiveRuntime))
			emitRunEvent(options, "info", "configuration reloaded")
		case tick := <-networkTicker.C:
			nextInterfaceServers, err := discoverInterfaceDNS(ctx, runtime, runner)
			if err != nil {
				if networkState.failed(err) {
					emitRunEvent(options, "warn", "network DNS discovery pending: "+err.Error())
				}
				continue
			}
			nextEffective := runtimeWithInterfaceDNS(runtime, nextInterfaceServers)
			fingerprint := networkFingerprint(nextEffective)
			wokeFromSleep := time.Since(tick) > 2*runtimeNetworkPoll
			if !networkState.shouldAttempt(fingerprint, wokeFromSleep) {
				continue
			}
			if runtime.Capture.DefaultRoute {
				nextPlan, err := planDefaultRouteCaptureOwned(ctx, nextEffective, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped, defaultRoutes.Bypasses)
				if err != nil || !defaultRoutes.equal(nextPlan) {
					primary = fmt.Errorf("default-route bypass topology changed; stopping for safe rollback: %w", errors.Join(err, errors.New("restart is required to rebuild bypass routes")))
					continue
				}
			}
			err = refreshNetwork(nextEffective, dataPlane, dnsServer)
			monitor.networkResult(time.Now().UTC(), err)
			if err != nil {
				if networkState.failed(err) {
					emitRunEvent(options, "warn", "network refresh pending: "+err.Error())
				}
				continue
			}
			dnsChanged := !sameInterfaceDNS(interfaceServers, nextInterfaceServers)
			interfaceServers = nextInterfaceServers
			if dnsChanged {
				for _, message := range effectiveDNSMessages(runtime, interfaceServers) {
					emitRunEvent(options, "info", message)
				}
			}
			networkState.succeeded(fingerprint)
			emitRunEvent(options, "info", "network state refreshed")
		case err := <-pumpDone:
			pumpConsumed = true
			if err != nil {
				primary = err
			} else {
				primary = errors.New("TUN packet pump stopped unexpectedly")
			}
		case err := <-outputDone:
			outputConsumed = true
			if err != nil {
				primary = err
			} else {
				primary = errors.New("netstack output pump stopped unexpectedly")
			}
		}
	}
	if errors.Is(primary, errRunStopped) {
		primary = nil
	}
	return finish(primary)
}

var errRunStopped = errors.New("run stopped")

func reloadRuntime(
	ctx context.Context,
	current *config.Config,
	digest *string,
	state *system.State,
	options RunOptions,
	plane *dataPlane,
	dnsServer *fakedns.Server,
	runner system.CommandRunner,
	defaultRoutes defaultRoutePlan,
	ipv6Enabled bool,
) (*config.Config, *config.Config, interfaceDNS, error) {
	if options.LoadConfig == nil {
		return nil, nil, nil, errors.New("configuration reload is not configured")
	}
	next, nextDigest, err := options.LoadConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	if nextDigest == "" {
		return nil, nil, nil, errors.New("reloaded config digest is empty")
	}
	if err := PreflightReload(ctx, current, next); err != nil {
		return nil, nil, nil, err
	}
	interfaceServers, err := discoverInterfaceDNS(ctx, next, runner)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discover reloaded interface DHCP DNS: %w", err)
	}
	nextEffective := runtimeWithInterfaceDNS(next, interfaceServers)
	if next.Capture.DefaultRoute {
		nextPlan, planErr := planDefaultRouteCaptureOwned(ctx, nextEffective, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped, defaultRoutes.Bypasses)
		if planErr != nil || !defaultRoutes.equal(nextPlan) {
			return nil, nil, nil, fmt.Errorf("reloaded default-route bypass topology differs from installed routes: %w", errors.Join(planErr, errors.New("restart is required to rebuild bypass routes")))
		}
	}
	nextGeneration, err := plane.prepare(nextEffective)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build reloaded data plane: %w", err)
	}
	defaultOutbound := nextEffective.Outbounds[nextEffective.DNS.DefaultOutbound]
	nextResolver, err := resolver.NewClient(defaultOutbound.Interface, defaultOutbound.DNS, runtimeDNSQueryTimeout, next.DNS.MaxConcurrent)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build reloaded Fake DNS resolver: %w", err)
	}
	previousDigest := state.ConfigDigest
	state.ConfigDigest = nextDigest
	if err := system.WriteState(current.System.StateFile, *state); err != nil {
		state.ConfigDigest = previousDigest
		return nil, nil, nil, fmt.Errorf("persist reloaded config digest: %w", err)
	}
	if err := dnsServer.Reload(next.FakeIP.DNSTTL, runtimeDNSQueryTimeout, next.FakeIP.Exclude, nextResolver); err != nil {
		state.ConfigDigest = previousDigest
		_ = system.WriteState(current.System.StateFile, *state)
		return nil, nil, nil, err
	}
	plane.commit(nextGeneration)
	configureLogging(next.Log)
	*digest = nextDigest
	return next, nextEffective, interfaceServers, nil
}

func emitRunEvent(options RunOptions, level, message string) {
	if options.Event != nil {
		options.Event(level, message)
		return
	}
	if level == "warn" {
		slog.Warn(message)
	} else {
		slog.Info(message)
	}
}

type packetReader interface {
	ReadPacket(context.Context) ([]byte, error)
}

type packetWriter interface {
	Write(context.Context, []byte) error
}

func pumpNetstackOutput(ctx context.Context, reader packetReader, writer packetWriter) error {
	for {
		packet, err := reader.ReadPacket(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("read netstack output: %w", err)
		}
		if len(packet) == 0 {
			return errors.New("netstack emitted malformed IP packet")
		}
		switch packet[0] >> 4 {
		case 4:
			if len(packet) < 20 {
				return errors.New("netstack emitted malformed IPv4 packet")
			}
		case 6:
			if len(packet) < 40 {
				return errors.New("netstack emitted malformed IPv6 packet")
			}
		default:
			return errors.New("netstack emitted packet with unsupported IP version")
		}
		if err := writer.Write(ctx, packet); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("write netstack packet to TUN: %w", err)
		}
	}
}
