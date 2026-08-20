//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/fakedns"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/privsep"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
)

// ServiceWorker is the non-root half of the managed LaunchDaemon. It owns the
// inherited data-plane descriptors and worker-writable persistence/status
// files, but never mutates host routes, system DNS, state, or lock files.
type ServiceWorker struct {
	resources *privsep.WorkerResources

	mutex         sync.Mutex
	runtime       *config.Config
	activeRuntime *config.Config
	digest        string
	ipv6Enabled   bool
	prepared      bool
	committed     bool
	monitor       *runMonitor
	fakePool      *fakeip.Pool
	fakeIPv6Pool  *fakeip.Pool
	persistence   *fakeip.Persistence
	persistence6  *fakeip.Persistence
	plane         *dataPlane
	stack         *netstack.Stack
	dnsServer     *fakedns.Server
	runningDNS    *fakedns.Running
	statusServer  *runtimestatus.Server
	packetPump    *internaltun.Pump

	dataCtx    context.Context
	cancelData context.CancelFunc
	wait       sync.WaitGroup
	done       chan error
	closing    atomic.Bool
	closeOnce  sync.Once
	closeErr   error
}

func NewServiceWorker(resources *privsep.WorkerResources) (*ServiceWorker, error) {
	if resources == nil || resources.Control == nil || resources.Device == nil || resources.DNS.UDP == nil || resources.DNS.TCP == nil {
		return nil, errors.New("complete inherited worker resources are required")
	}
	return &ServiceWorker{resources: resources, done: make(chan error, 1)}, nil
}

func (worker *ServiceWorker) Prepare(ctx context.Context, bootstrap privsep.Bootstrap) error {
	worker.mutex.Lock()
	defer worker.mutex.Unlock()
	if worker.prepared {
		return errors.New("service worker is already prepared")
	}
	runtime, digest, err := config.LoadBytesWithDigest(bootstrap.Config)
	if err != nil {
		return err
	}
	if digest != bootstrap.ConfigDigest {
		return fmt.Errorf("compiled config digest=%q, want %q", digest, bootstrap.ConfigDigest)
	}
	if runtime.DNS.Listen.String() != bootstrap.DNSListen {
		return fmt.Errorf("config DNS listen=%s, inherited bootstrap=%s", runtime.DNS.Listen, bootstrap.DNSListen)
	}
	if !runtime.DNS.UDP || !runtime.DNS.TCP {
		return errors.New("managed service worker requires both UDP and TCP Fake DNS listeners")
	}
	if worker.resources.Device.Name() != bootstrap.TUNName {
		return fmt.Errorf("inherited TUN name=%s, bootstrap=%s", worker.resources.Device.Name(), bootstrap.TUNName)
	}
	mtu, err := worker.resources.Device.Native().MTU()
	if err != nil {
		return fmt.Errorf("read inherited TUN MTU: %w", err)
	}
	if mtu != runtime.TUN.MTU {
		return fmt.Errorf("inherited TUN MTU=%d, config=%d", mtu, runtime.TUN.MTU)
	}
	if got := worker.resources.DNS.UDP.LocalAddr().String(); got != bootstrap.DNSListen {
		return fmt.Errorf("inherited UDP DNS listen=%s, bootstrap=%s", got, bootstrap.DNSListen)
	}
	if got := worker.resources.DNS.TCP.Addr().String(); got != bootstrap.DNSListen {
		return fmt.Errorf("inherited TCP DNS listen=%s, bootstrap=%s", got, bootstrap.DNSListen)
	}
	effectiveRuntime := runtimeWithInterfaceDNS(runtime, interfaceDNS(bootstrap.InterfaceDNS))

	configureLogging(runtime.Log)
	fakePool, err := fakeip.New(runtime.FakeIP.Prefix, runtime.FakeIP.MappingTTL, runtime.FakeIP.MaxMappings, 10)
	if err != nil {
		return err
	}
	persistence, quarantined, err := fakeip.OpenPersistence(runtime.FakeIP.PersistenceFile, fakePool, runtime.FakeIP.DNSTTL)
	if err != nil {
		return fmt.Errorf("open Fake IP persistence: %w", err)
	}
	if quarantined != "" {
		slog.Warn("quarantined invalid Fake IP persistence", "path", quarantined)
	}
	var fakeIPv6Pool *fakeip.Pool
	var persistence6 *fakeip.Persistence
	if runtime.FakeIPv6 != nil {
		fakeIPv6Pool, err = fakeip.New(runtime.FakeIPv6.Prefix, runtime.FakeIP.MappingTTL, runtime.FakeIPv6.MaxMappings, 10)
		if err != nil {
			return fmt.Errorf("create Fake IPv6 pool: %w", err)
		}
		persistence6, quarantined, err = fakeip.OpenPersistence(
			runtime.FakeIPv6.PersistenceFile, fakeIPv6Pool, runtime.FakeIP.DNSTTL,
		)
		if err != nil {
			return fmt.Errorf("open Fake IPv6 persistence: %w", err)
		}
		if quarantined != "" {
			slog.Warn("quarantined invalid Fake IPv6 persistence", "path", quarantined)
		}
	}
	activeIPv6Pool := fakeIPv6Pool
	if !bootstrap.IPv6Enabled {
		activeIPv6Pool = nil
	}
	plane, err := newDataPlane(fakePool, activeIPv6Pool, effectiveRuntime)
	if err != nil {
		return err
	}
	stack, err := netstack.New(netstack.Config{
		MTU: runtime.TUN.MTU, PacketQueue: runtime.TUN.PacketQueue, MaxTCPFlows: runtime.Sessions.MaxTCPFlows,
	}, plane.handleTCP, plane.handleUDP)
	if err != nil {
		return err
	}
	defaultOutbound := effectiveRuntime.Outbounds[effectiveRuntime.DNS.DefaultOutbound]
	forwardResolver, err := resolver.NewClient(defaultOutbound.Interface, defaultOutbound.DNS, runtimeDNSQueryTimeout, runtime.DNS.MaxConcurrent)
	if err != nil {
		_ = stack.Close()
		return fmt.Errorf("build Fake DNS forwarding resolver: %w", err)
	}
	dnsServer, err := fakedns.NewDualStack(fakedns.Config{
		Listen: runtime.DNS.Listen, UDP: true, TCP: true, TTL: runtime.FakeIP.DNSTTL,
		QueryTimeout: runtimeDNSQueryTimeout, MaxConcurrent: runtime.DNS.MaxConcurrent,
	}, fakePool, activeIPv6Pool, runtime.FakeIP.Exclude, forwardResolver)
	if err != nil {
		_ = stack.Close()
		return err
	}
	packetBuffers, err := internaltun.NewBufferPool(internaltun.PacketOffset+runtime.TUN.MTU+256, runtime.TUN.BufferPool)
	if err != nil {
		_ = stack.Close()
		return err
	}
	packetPump, err := internaltun.NewPump(worker.resources.Device, packetBuffers, func(_ context.Context, packet []byte) error {
		return stack.InjectPacket(packet)
	})
	if err != nil {
		_ = stack.Close()
		return err
	}

	worker.runtime = runtime
	worker.activeRuntime = effectiveRuntime
	worker.digest = digest
	worker.ipv6Enabled = bootstrap.IPv6Enabled
	worker.monitor = newRunMonitor(time.Now().UTC(), digest, runtime, bootstrap.IPv6Enabled, bootstrap.IPv6FallbackReason)
	worker.fakePool = fakePool
	worker.fakeIPv6Pool = fakeIPv6Pool
	worker.persistence = persistence
	worker.persistence6 = persistence6
	worker.plane = plane
	worker.stack = stack
	worker.dnsServer = dnsServer
	worker.packetPump = packetPump
	worker.dataCtx, worker.cancelData = context.WithCancel(context.Background())

	worker.wait.Add(2)
	go worker.runComponent(worker.dataCtx, "TUN packet pump", func() error { return packetPump.Run(worker.dataCtx) })
	go worker.runComponent(worker.dataCtx, "netstack output pump", func() error {
		return pumpNetstackOutput(worker.dataCtx, stack, packetPump)
	})
	runningDNS, err := dnsServer.StartWithListeners(worker.resources.DNS)
	if err != nil {
		worker.cancelData()
		_ = worker.resources.Device.Close()
		worker.wait.Wait()
		_ = stack.Close()
		return err
	}
	select {
	case <-runningDNS.Done():
		closeCtx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
		closeErr := runningDNS.Close(closeCtx)
		cancel()
		worker.cancelData()
		_ = worker.resources.Device.Close()
		worker.wait.Wait()
		_ = stack.Close()
		return errors.Join(runningDNS.Err(), closeErr)
	default:
	}
	worker.runningDNS = runningDNS
	worker.resources.DNS = fakedns.Listeners{}
	worker.wait.Add(1)
	go worker.runComponent(worker.dataCtx, "Fake DNS", func() error {
		select {
		case <-runningDNS.Done():
			return runningDNS.Err()
		case <-worker.dataCtx.Done():
			return worker.dataCtx.Err()
		}
	})
	statusServer, err := runtimestatus.Start(bootstrap.StatusSocket, worker.snapshot)
	if err != nil {
		worker.cancelData()
		closeCtx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
		_ = runningDNS.Close(closeCtx)
		cancel()
		worker.runningDNS = nil
		_ = worker.resources.Device.Close()
		worker.wait.Wait()
		_ = stack.Close()
		return err
	}
	worker.statusServer = statusServer
	worker.prepared = true
	return nil
}

func (worker *ServiceWorker) Commit(context.Context) error {
	worker.mutex.Lock()
	defer worker.mutex.Unlock()
	if !worker.prepared {
		return errors.New("service worker is not prepared")
	}
	if worker.committed {
		return errors.New("service worker is already committed")
	}
	if worker.runningDNS != nil {
		select {
		case <-worker.runningDNS.Done():
			if err := worker.runningDNS.Err(); err != nil {
				return err
			}
			return errors.New("Fake DNS stopped unexpectedly")
		default:
		}
	}
	select {
	case err := <-worker.done:
		if err == nil {
			err = errors.New("worker component stopped unexpectedly")
		}
		return err
	default:
	}
	worker.committed = true
	worker.wait.Add(1)
	go worker.monitorNetwork()
	return nil
}

func (worker *ServiceWorker) Reload(ctx context.Context, reload privsep.Reload) error {
	worker.mutex.Lock()
	defer worker.mutex.Unlock()
	if !worker.committed {
		return errors.New("service worker is not running")
	}
	next, digest, err := config.LoadBytesWithDigest(reload.Config)
	if err == nil && digest != reload.ConfigDigest {
		err = fmt.Errorf("compiled reload digest=%q, want %q", digest, reload.ConfigDigest)
	}
	if err == nil {
		err = PreflightReload(ctx, worker.runtime, next)
	}
	nextEffective := runtimeWithInterfaceDNS(next, interfaceDNS(reload.InterfaceDNS))
	var nextGeneration *dataPlaneGeneration
	if err == nil {
		nextGeneration, err = worker.plane.prepare(nextEffective)
		if err != nil {
			err = fmt.Errorf("build reloaded data plane: %w", err)
		}
	}
	var nextResolver *resolver.Client
	if err == nil {
		defaultOutbound := nextEffective.Outbounds[nextEffective.DNS.DefaultOutbound]
		nextResolver, err = resolver.NewClient(defaultOutbound.Interface, defaultOutbound.DNS, runtimeDNSQueryTimeout, next.DNS.MaxConcurrent)
		if err != nil {
			err = fmt.Errorf("build reloaded Fake DNS resolver: %w", err)
		}
	}
	if err == nil {
		err = worker.dnsServer.Reload(next.FakeIP.DNSTTL, runtimeDNSQueryTimeout, next.FakeIP.Exclude, nextResolver)
	}
	if err == nil {
		worker.plane.commit(nextGeneration)
		worker.runtime = next
		worker.activeRuntime = nextEffective
		worker.digest = digest
		configureLogging(next.Log)
	}
	worker.monitor.reloadResult(time.Now().UTC(), worker.digest, next, err)
	return err
}

func (worker *ServiceWorker) Done() <-chan error { return worker.done }

func (worker *ServiceWorker) Close(ctx context.Context) error {
	worker.closeOnce.Do(func() {
		worker.closing.Store(true)
		worker.mutex.Lock()
		statusServer := worker.statusServer
		worker.statusServer = nil
		persistence := worker.persistence
		persistence6 := worker.persistence6
		runningDNS := worker.runningDNS
		worker.runningDNS = nil
		cancelData := worker.cancelData
		device := worker.resources.Device
		worker.resources.Device = nil
		stack := worker.stack
		worker.mutex.Unlock()

		var failures []error
		if statusServer != nil {
			failures = append(failures, statusServer.Close(ctx))
		}
		if runningDNS != nil {
			failures = append(failures, runningDNS.Close(ctx))
		}
		if cancelData != nil {
			cancelData()
		}
		if device != nil {
			failures = append(failures, device.Close())
		}
		waitDone := make(chan struct{})
		go func() {
			worker.wait.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-ctx.Done():
			failures = append(failures, ctx.Err())
		}
		if stack != nil {
			failures = append(failures, stack.Close())
		}
		if persistence != nil {
			failures = append(failures, persistence.Flush())
		}
		if persistence6 != nil {
			failures = append(failures, persistence6.Flush())
		}
		worker.closeErr = errors.Join(failures...)
	})
	return worker.closeErr
}

func (worker *ServiceWorker) runComponent(ctx context.Context, name string, run func() error) {
	defer worker.wait.Done()
	err := run()
	if worker.closing.Load() || ctx.Err() != nil {
		return
	}
	if err == nil {
		err = fmt.Errorf("%s stopped unexpectedly", name)
	} else {
		err = fmt.Errorf("%s: %w", name, err)
	}
	select {
	case worker.done <- err:
	default:
	}
}

func (worker *ServiceWorker) monitorNetwork() {
	defer worker.wait.Done()
	ticker := time.NewTicker(runtimeNetworkPoll)
	defer ticker.Stop()
	worker.mutex.Lock()
	state := newNetworkRefreshState(networkFingerprint(worker.activeRuntime))
	worker.mutex.Unlock()
	for {
		select {
		case <-worker.dataCtx.Done():
			return
		case tick := <-ticker.C:
			worker.mutex.Lock()
			fingerprint := networkFingerprint(worker.activeRuntime)
			wokeFromSleep := time.Since(tick) > 2*runtimeNetworkPoll
			if !state.shouldAttempt(fingerprint, wokeFromSleep) {
				worker.mutex.Unlock()
				continue
			}
			err := refreshNetwork(worker.activeRuntime, worker.plane, worker.dnsServer)
			worker.monitor.networkResult(time.Now().UTC(), err)
			if err != nil {
				if state.failed(err) {
					slog.Warn("worker network refresh pending", "error", err)
				}
			} else {
				state.succeeded(fingerprint)
				slog.Info("worker network state refreshed")
			}
			worker.mutex.Unlock()
		}
	}
}

func (worker *ServiceWorker) snapshot() runtimestatus.Snapshot {
	worker.mutex.Lock()
	monitor := worker.monitor
	stack := worker.stack
	plane := worker.plane
	dnsServer := worker.dnsServer
	fakePool := worker.fakePool
	fakeIPv6Pool := worker.fakeIPv6Pool
	packetPump := worker.packetPump
	worker.mutex.Unlock()

	var snapshot runtimestatus.Snapshot
	if monitor != nil {
		snapshot = monitor.snapshot()
	}
	snapshot.Resources = runtimestatus.Resources()
	if stack != nil {
		snapshot.Netstack = stack.Stats()
	}
	if plane != nil {
		snapshot.TCP, snapshot.UDP = plane.stats()
	}
	if dnsServer != nil {
		snapshot.DNS = dnsServer.Stats()
	}
	if fakePool != nil {
		snapshot.FakeIP = fakePool.Stats()
	}
	if fakeIPv6Pool != nil {
		snapshot.FakeIPv6 = fakeIPv6Pool.Stats()
	}
	if packetPump != nil {
		snapshot.TUN = packetPump.Stats()
	}
	return snapshot
}
