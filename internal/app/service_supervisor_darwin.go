//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/hailinpan/tun-proxy/internal/apperror"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/control"
	"github.com/hailinpan/tun-proxy/internal/daemon"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	"github.com/hailinpan/tun-proxy/internal/system"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
	"golang.org/x/sys/unix"
)

const serviceSupervisorTimeout = 30 * time.Second

type ServiceSupervisorOptions struct {
	Reload <-chan struct{}
	Ready  func(tunName string)
	Event  func(level, message string)
}

type supervisorReloadRequest struct {
	context        context.Context
	requestID      string
	operationID    string
	rollbackOf     string
	expectedDigest string
	reply          chan supervisorReloadResult
}

type supervisorReloadResult struct {
	digest string
	err    error
	fatal  bool
}

// RunServiceSupervisor is the root half of the managed LaunchDaemon. It owns
// every privileged descriptor and host mutation while a dedicated non-root
// worker owns the data plane and worker-writable runtime files.
func RunServiceSupervisor(ctx context.Context, configPath string, layout launchservice.Layout, options ServiceSupervisorOptions) error {
	if ctx == nil {
		return errors.New("service supervisor context is required")
	}
	if err := system.RequireRoot(); err != nil {
		return err
	}
	if err := layout.Validate(); err != nil {
		return err
	}
	if configPath != layout.Config {
		return fmt.Errorf("managed service requires config %q", layout.Config)
	}
	if err := launchservice.PrepareSupervisorRuntime(layout, 0); err != nil {
		return err
	}
	identity, err := launchservice.ResolveWorkerIdentity(ctx)
	if err != nil {
		return err
	}
	if err := launchservice.PrepareEphemeralWorkerRuntime(layout, 0, identity); err != nil {
		return fmt.Errorf("prepare managed worker runtime: %w", err)
	}
	if err := launchservice.ValidateWorkerStorage(layout, identity); err != nil {
		return err
	}
	if err := CleanupWithStatusOwners(context.Background(), layout.State, layout.Lock, 0, identity.UID); err != nil {
		return fmt.Errorf("recover stale service state: %w", err)
	}
	configBytes, runtime, digest, err := loadManagedServiceConfig(configPath, layout)
	if err != nil {
		return err
	}
	runner := system.NativeCommandRunner{}
	interfaceServers, err := discoverInterfaceDNS(ctx, runtime, runner)
	if err != nil {
		emitServiceSupervisorEvent(options, "warn", "discover interface DHCP DNS: "+err.Error()+"; configured DNS will be used where discovery failed")
	}
	effectiveRuntime := runtimeWithInterfaceDNS(runtime, interfaceServers)
	for _, message := range effectiveDNSMessages(runtime, interfaceServers) {
		emitServiceSupervisorEvent(options, "info", message)
	}
	if err := checkInterfaces(runtime); err != nil {
		return err
	}
	if err := system.CheckPrefixAvailable(ctx, runtime.FakeIP.Prefix); err != nil {
		return err
	}
	ipv6Enabled, ipv6FallbackReason := IPv6DataPathAvailable(ctx, runtime)
	if runtime.FakeIPv6 != nil && !ipv6Enabled {
		emitServiceSupervisorEvent(options, "warn", "IPv6 data path disabled: "+ipv6FallbackReason+"; Fake AAAA will return NODATA until restart")
	}
	if ipv6Enabled {
		if err := system.CheckPrefixAvailable(ctx, runtime.FakeIPv6.Prefix); err != nil {
			return err
		}
	}
	defaultRoutes, err := planDefaultRouteCapture(ctx, effectiveRuntime, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped)
	if err != nil {
		return err
	}

	if _, err := os.Lstat(layout.State); err == nil {
		return fmt.Errorf("recovery state %q already exists after cleanup", layout.State)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect recovery state %q: %w", layout.State, err)
	}
	if err := runtimestatus.RemoveStaleForOwners(layout.StatusSocket, 0, identity.UID); err != nil {
		return fmt.Errorf("remove stale worker status socket: %w", err)
	}
	if err := control.RemoveStaleForOwner(layout.ControlSocket, 0); err != nil {
		return fmt.Errorf("remove stale supervisor control socket: %w", err)
	}
	lock, err := daemon.Acquire(layout.Lock)
	if err != nil {
		return err
	}
	state := system.NewState(digest)
	state.LockFile = layout.Lock
	stateExists := false
	var device *internaltun.Device
	var udpDNS *net.UDPConn
	var tcpDNS *net.TCPListener
	var handoff *privsep.Handoff
	var command *exec.Cmd
	var session *privsep.SupervisorSession
	var controlServer *control.Server
	workerStarted := false
	workerRunning := false

	finish := func(primary error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), serviceSupervisorTimeout)
		defer cancel()
		var failures []error
		if stateExists {
			state.Phase = "stopping"
			if err := system.WriteState(layout.State, state); err != nil {
				failures = append(failures, fmt.Errorf("persist stopping state: %w", err))
			}
		}
		if controlServer != nil {
			if err := controlServer.Close(cleanupCtx); err != nil {
				failures = append(failures, fmt.Errorf("close supervisor control socket: %w", err))
			} else {
				state.ControlSocket = ""
				if stateExists {
					if err := system.WriteState(layout.State, state); err != nil {
						failures = append(failures, fmt.Errorf("persist control socket cleanup: %w", err))
					}
				}
			}
		}
		// Restore host DNS and remove routes while the worker still serves DNS
		// and retains its inherited utun descriptor.
		if len(state.DNS) != 0 {
			remaining, err := system.RestoreDNS(cleanupCtx, runner, state.DNS)
			state.DNS = remaining
			if err != nil {
				failures = append(failures, fmt.Errorf("restore system DNS: %w", err))
			}
			if stateExists {
				if err := system.WriteState(layout.State, state); err != nil {
					failures = append(failures, fmt.Errorf("persist DNS rollback: %w", err))
				}
			}
		}
		if stateExists && (state.Route != nil || len(state.Routes) != 0) {
			if err := removeRecordedRoutes(cleanupCtx, runner, layout.State, &state); err != nil {
				failures = append(failures, err)
			}
		}
		if workerStarted {
			if workerRunning && session != nil {
				if err := session.Shutdown(cleanupCtx, "service supervisor stopping"); err != nil {
					failures = append(failures, fmt.Errorf("stop service worker: %w", err))
					if command != nil && command.Process != nil {
						_ = command.Process.Kill()
					}
				}
			} else if command != nil && command.Process != nil {
				_ = command.Process.Kill()
			}
			if session != nil {
				if err := session.WaitProcess(cleanupCtx); err != nil && !workerRunning {
					// A killed worker commonly reports signal termination; startup's
					// primary error is the useful failure in that case.
				} else if err != nil {
					failures = append(failures, fmt.Errorf("wait for service worker: %w", err))
				}
			}
		}
		if handoff != nil {
			failures = append(failures, handoff.Close())
		}
		if tcpDNS != nil {
			failures = append(failures, tcpDNS.Close())
		}
		if udpDNS != nil {
			failures = append(failures, udpDNS.Close())
		}
		if device != nil {
			failures = append(failures, device.Close())
		}
		if err := runtimestatus.RemoveStaleForOwners(layout.StatusSocket, 0, identity.UID); err != nil {
			failures = append(failures, fmt.Errorf("remove worker status socket: %w", err))
		} else {
			state.StatusSocket = ""
			if stateExists {
				if err := system.WriteState(layout.State, state); err != nil {
					failures = append(failures, fmt.Errorf("persist status cleanup: %w", err))
				}
			}
		}
		if err := lock.Close(); err != nil {
			failures = append(failures, fmt.Errorf("release process lock: %w", err))
		}
		cleanupErr := errors.Join(failures...)
		if cleanupErr == nil && stateExists {
			cleanupErr = system.RemoveState(layout.State)
		}
		return errors.Join(primary, cleanupErr)
	}

	if err := system.WriteState(layout.State, state); err != nil {
		_ = lock.Close()
		return err
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
	if err := system.WriteState(layout.State, state); err != nil {
		return finish(err)
	}
	udpDNS, tcpDNS, err = bindManagedDNS(runtime.DNS.Listen)
	if err != nil {
		return finish(err)
	}
	handoff, err = privsep.PrepareHandoff(device.Native().File(), udpDNS, tcpDNS)
	if err != nil {
		return finish(err)
	}
	command, err = privsep.WorkerCommand(layout.Binary, identity, handoff)
	if err != nil {
		return finish(err)
	}
	processDone := make(chan error, 1)
	if err := command.Start(); err != nil {
		return finish(fmt.Errorf("start service worker: %w", err))
	}
	workerStarted = true
	go func() {
		processDone <- command.Wait()
		close(processDone)
	}()
	if err := handoff.CloseChildFiles(); err != nil {
		return finish(fmt.Errorf("close worker descriptor copies: %w", err))
	}
	session, err = privsep.NewSupervisorSession(handoff.Control, command.Process.Pid, identity, processDone)
	if err != nil {
		return finish(err)
	}
	bootstrapCtx, cancelBootstrap := context.WithTimeout(ctx, serviceSupervisorTimeout)
	_, err = session.Bootstrap(bootstrapCtx, privsep.Bootstrap{
		Config: configBytes, ConfigDigest: digest, TUNName: device.Name(),
		DNSListen: runtime.DNS.Listen.String(), StatusSocket: layout.StatusSocket,
		IPv6Enabled: ipv6Enabled, IPv6FallbackReason: ipv6FallbackReason,
		InterfaceDNS: interfaceServers,
	})
	cancelBootstrap()
	if err != nil {
		return finish(err)
	}

	if err := installServiceRoutes(ctx, runner, layout.State, &state, runtime, device.Name(), ipv6Enabled, defaultRoutes); err != nil {
		return finish(err)
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
		if err := system.WriteState(layout.State, state); err != nil {
			return finish(err)
		}
		if _, err := system.ApplyDNS(ctx, runner, planned, runtime.DNS.Listen.Addr()); err != nil {
			return finish(err)
		}
	}
	commitCtx, cancelCommit := context.WithTimeout(ctx, serviceSupervisorTimeout)
	err = session.Commit(commitCtx, digest)
	cancelCommit()
	if err != nil {
		return finish(err)
	}
	workerRunning = true
	reloadRequests := make(chan supervisorReloadRequest)
	controlServer, err = control.Start(layout.ControlSocket, 0, func(requestCtx context.Context, controlRequest control.ReloadRequest) (string, error) {
		reply := make(chan supervisorReloadResult)
		request := supervisorReloadRequest{
			context: requestCtx, requestID: controlRequest.RequestID, operationID: controlRequest.OperationID,
			rollbackOf: controlRequest.RollbackOf, expectedDigest: controlRequest.ExpectedConfigDigest, reply: reply,
		}
		select {
		case reloadRequests <- request:
		case <-requestCtx.Done():
			return "", requestCtx.Err()
		}
		select {
		case result := <-reply:
			return result.digest, result.err
		case <-requestCtx.Done():
			return "", requestCtx.Err()
		}
	})
	if err != nil {
		return finish(fmt.Errorf("start supervisor control socket: %w", err))
	}
	state.StatusSocket = layout.StatusSocket
	state.ControlSocket = layout.ControlSocket
	state.Phase = "running"
	if err := system.WriteState(layout.State, state); err != nil {
		return finish(err)
	}
	if options.Ready != nil {
		options.Ready(device.Name())
	}
	networkTicker := time.NewTicker(runtimeNetworkPoll)
	defer networkTicker.Stop()
	networkState := newNetworkRefreshState(networkFingerprint(effectiveRuntime))

	performReload := func(requestCtx context.Context, request supervisorReloadRequest) supervisorReloadResult {
		nextBytes, next, nextDigest, reloadErr := loadManagedServiceConfig(configPath, layout)
		if reloadErr == nil {
			reloadErr = validateExpectedReloadDigest(nextDigest, request.expectedDigest)
		}
		if reloadErr == nil {
			reloadErr = PreflightReload(requestCtx, runtime, next)
		}
		var nextInterfaceServers interfaceDNS
		var nextEffective *config.Config
		if reloadErr == nil {
			nextInterfaceServers, reloadErr = discoverInterfaceDNS(requestCtx, next, runner)
			nextEffective = runtimeWithInterfaceDNS(next, nextInterfaceServers)
		}
		if reloadErr == nil && next.Capture.DefaultRoute {
			nextPlan, planErr := planDefaultRouteCaptureOwned(requestCtx, nextEffective, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped, defaultRoutes.Bypasses)
			if planErr != nil || !defaultRoutes.equal(nextPlan) {
				reloadErr = apperror.Wrap(
					apperror.CodeConfigRestartRequired,
					"service.reload",
					"default-route bypass topology changed and requires a service restart",
					errors.Join(planErr, errors.New("restart is required to rebuild bypass routes")),
				)
			}
		}
		if reloadErr == nil {
			reloadCtx, cancel := context.WithTimeout(requestCtx, serviceSupervisorTimeout)
			reloadErr = session.Reload(reloadCtx, privsep.Reload{
				ReloadRequestID: request.requestID, Config: nextBytes,
				ConfigDigest: nextDigest, InterfaceDNS: nextInterfaceServers,
			})
			cancel()
		}
		if reloadErr != nil {
			return supervisorReloadResult{err: annotateSupervisorReloadError(reloadErr, request, "apply")}
		}
		previousDigest := state.ConfigDigest
		state.ConfigDigest = nextDigest
		if reloadErr := system.WriteState(layout.State, state); reloadErr != nil {
			state.ConfigDigest = previousDigest
			reloadErr = apperror.Wrap(
				apperror.CodeInternalError,
				"service.reload",
				"configuration reload activated but service state could not be updated",
				reloadErr,
			)
			return supervisorReloadResult{err: annotateSupervisorReloadError(reloadErr, request, "persist"), fatal: true}
		}
		runtime = next
		effectiveRuntime = nextEffective
		interfaceServers = nextInterfaceServers
		configBytes = nextBytes
		digest = nextDigest
		for _, message := range effectiveDNSMessages(runtime, interfaceServers) {
			emitServiceSupervisorEvent(options, "info", message)
		}
		networkState.reset(networkFingerprint(effectiveRuntime))
		emitServiceSupervisorEvent(options, "info", fmt.Sprintf(
			"configuration reloaded request_id=%s operation_id=%s rollback_of=%s config_digest=%s",
			request.requestID, request.operationID, request.rollbackOf, nextDigest,
		))
		return supervisorReloadResult{digest: nextDigest}
	}

	for {
		select {
		case <-ctx.Done():
			return finish(nil)
		case <-session.ProcessExited():
			exitErr := session.WaitProcess(context.Background())
			if exitErr == nil {
				exitErr = errors.New("service worker exited unexpectedly")
			}
			workerRunning = false
			return finish(fmt.Errorf("service worker exited: %w", exitErr))
		case request := <-reloadRequests:
			result := performReload(request.context, request)
			select {
			case request.reply <- result:
			case <-request.context.Done():
			}
			if result.err != nil {
				emitServiceSupervisorEvent(options, "warn", fmt.Sprintf(
					"configuration reload rejected request_id=%s operation_id=%s rollback_of=%s: %v",
					request.requestID, request.operationID, request.rollbackOf, result.err,
				))
			}
			if result.fatal {
				return finish(result.err)
			}
		case <-options.Reload:
			request, requestErr := newSupervisorReloadRequest(ctx, "")
			if requestErr != nil {
				emitServiceSupervisorEvent(options, "warn", "generate SIGHUP reload request ID: "+requestErr.Error())
				continue
			}
			result := performReload(ctx, request)
			if result.err != nil {
				emitServiceSupervisorEvent(options, "warn", fmt.Sprintf(
					"configuration reload rejected request_id=%s operation_id=%s source=sighup: %v",
					request.requestID, request.operationID, result.err,
				))
			}
			if result.fatal {
				return finish(result.err)
			}
		case tick := <-networkTicker.C:
			nextInterfaceServers, err := discoverInterfaceDNS(ctx, runtime, runner)
			if err != nil {
				if networkState.failed(err) {
					emitServiceSupervisorEvent(options, "warn", "network DNS discovery pending: "+err.Error())
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
				nextPlan, planErr := planDefaultRouteCaptureOwned(ctx, nextEffective, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped, defaultRoutes.Bypasses)
				if planErr != nil || !defaultRoutes.equal(nextPlan) {
					return finish(fmt.Errorf("default-route bypass topology changed; stopping for safe rollback: %w", errors.Join(planErr, errors.New("restart is required to rebuild bypass routes"))))
				}
			}
			dnsChanged := !sameInterfaceDNS(interfaceServers, nextInterfaceServers)
			if dnsChanged {
				requestID, requestErr := control.NewRequestID()
				if requestErr != nil {
					if networkState.failed(requestErr) {
						emitServiceSupervisorEvent(options, "warn", "generate worker DNS refresh request ID: "+requestErr.Error())
					}
					continue
				}
				reloadCtx, cancel := context.WithTimeout(ctx, serviceSupervisorTimeout)
				err = session.Reload(reloadCtx, privsep.Reload{
					ReloadRequestID: requestID, Config: configBytes, ConfigDigest: digest, InterfaceDNS: nextInterfaceServers,
				})
				cancel()
				if err != nil {
					if networkState.failed(err) {
						emitServiceSupervisorEvent(options, "warn", "worker DNS refresh pending: "+err.Error())
					}
					continue
				}
			}
			interfaceServers = nextInterfaceServers
			if dnsChanged {
				for _, message := range effectiveDNSMessages(runtime, interfaceServers) {
					emitServiceSupervisorEvent(options, "info", message)
				}
			}
			networkState.succeeded(fingerprint)
		}
	}
}

func newSupervisorReloadRequest(ctx context.Context, expectedDigest string) (supervisorReloadRequest, error) {
	requestID, err := control.NewRequestID()
	if err != nil {
		return supervisorReloadRequest{}, err
	}
	operationID, err := control.NewRequestID()
	if err != nil {
		return supervisorReloadRequest{}, err
	}
	return supervisorReloadRequest{
		context: ctx, requestID: requestID, operationID: operationID, expectedDigest: expectedDigest,
	}, nil
}

func validateExpectedReloadDigest(actual, expected string) error {
	if expected != "" && actual != expected {
		return apperror.Wrap(
			apperror.CodeReloadDigestMismatch,
			"service.reload",
			"managed configuration digest does not match the requested digest",
			fmt.Errorf("got %q, want %q", actual, expected),
		).WithDetails(map[string]any{
			"actual_config_digest":   actual,
			"expected_config_digest": expected,
		})
	}
	return nil
}

func annotateSupervisorReloadError(err error, request supervisorReloadRequest, phase string) error {
	if err == nil {
		return nil
	}
	code := apperror.CodeOf(err)
	var typed *apperror.Error
	isTyped := errors.As(err, &typed) && typed != nil
	requestTimedOut := request.context != nil && errors.Is(request.context.Err(), context.DeadlineExceeded)
	if errors.Is(err, context.DeadlineExceeded) || requestTimedOut {
		code = apperror.CodeReloadTimeout
	} else if !isTyped {
		code = apperror.CodeReloadRejected
	}
	return apperror.Wrap(code, "service.reload", "supervisor rejected configuration reload", err).WithDetails(map[string]any{
		"operation_id":      request.operationID,
		"reload_request_id": request.requestID,
		"rollback_of":       request.rollbackOf,
		"phase":             phase,
	})
}

func loadManagedServiceConfig(path string, layout launchservice.Layout) ([]byte, *config.Config, string, error) {
	return loadManagedServiceConfigForOwner(path, layout, 0)
}

func loadManagedServiceConfigForOwner(path string, layout launchservice.Layout, expectedUID uint32) ([]byte, *config.Config, string, error) {
	const operation = "service.reload"
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, "", apperror.Wrap(apperror.CodeConfigInvalid, operation, "managed configuration cannot be opened", fmt.Errorf("open managed config %q: %w", path, err))
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close() //nolint:errcheck // Best-effort cleanup.
	info, err := file.Stat()
	if err != nil {
		return nil, nil, "", apperror.Wrap(apperror.CodeUnsafeFile, operation, "managed configuration cannot be inspected safely", fmt.Errorf("inspect managed config %q: %w", path, err))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Uid != expectedUID {
		return nil, nil, "", apperror.Wrap(
			apperror.CodeUnsafeFile,
			operation,
			"managed configuration has unsafe ownership, type, or permissions",
			fmt.Errorf("managed config %q must be a UID %d-owned regular file with mode 0600", path, expectedUID),
		)
	}
	contents, err := io.ReadAll(io.LimitReader(file, privsep.MaxConfigSize+1))
	if err != nil {
		return nil, nil, "", apperror.Wrap(apperror.CodeConfigInvalid, operation, "managed configuration cannot be read", fmt.Errorf("read managed config %q: %w", path, err))
	}
	if len(contents) > privsep.MaxConfigSize {
		return nil, nil, "", apperror.Wrap(apperror.CodeConfigInvalid, operation, "managed configuration is too large", fmt.Errorf("managed config %q exceeds %d bytes", path, privsep.MaxConfigSize))
	}
	runtime, digest, err := config.LoadBytesWithDigest(contents)
	if err != nil {
		return nil, nil, "", apperror.Wrap(apperror.CodeConfigInvalid, operation, "managed configuration is invalid", err)
	}
	if err := launchservice.ValidateManagedConfig(runtime, layout); err != nil {
		return nil, nil, "", apperror.Wrap(apperror.CodeConfigInvalid, operation, "managed configuration is invalid for the service", err)
	}
	return contents, runtime, digest, nil
}

func bindManagedDNS(address netip.AddrPort) (*net.UDPConn, *net.TCPListener, error) {
	udp, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(address))
	if err != nil {
		return nil, nil, fmt.Errorf("bind managed UDP DNS %s: %w", address, err)
	}
	tcp, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(address))
	if err != nil {
		_ = udp.Close()
		return nil, nil, fmt.Errorf("bind managed TCP DNS %s: %w", address, err)
	}
	return udp, tcp, nil
}

func installServiceRoutes(ctx context.Context, runner system.CommandRunner, statePath string, state *system.State, runtime *config.Config, tunName string, ipv6Enabled bool, defaultRoutes defaultRoutePlan) error {
	add := func(route system.RouteState, legacy bool) error {
		if legacy {
			state.Route = &route
		} else {
			state.Routes = append(state.Routes, route)
		}
		if err := system.WriteState(statePath, *state); err != nil {
			return err
		}
		if err := system.AddRoute(ctx, runner, route); err != nil {
			return err
		}
		return system.VerifyRoute(ctx, runner, route)
	}
	if err := add(system.RouteState{Prefix: runtime.FakeIP.Prefix.String(), Interface: tunName}, true); err != nil {
		return err
	}
	if ipv6Enabled {
		if err := add(system.RouteState{Prefix: runtime.FakeIPv6.Prefix.String(), Interface: tunName}, false); err != nil {
			return err
		}
	}
	for _, route := range defaultRoutes.routes(tunName) {
		if err := add(route, false); err != nil {
			return err
		}
	}
	if runtime.Capture.DefaultRoute {
		verified, err := planDefaultRouteCaptureOwned(ctx, runtime, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped, defaultRoutes.Bypasses)
		if err != nil || !defaultRoutes.equal(verified) {
			return fmt.Errorf("prove loop-free egress after default-route capture: %w", errors.Join(err, errors.New("captured route plan changed during installation")))
		}
	}
	return nil
}

func emitServiceSupervisorEvent(options ServiceSupervisorOptions, level, message string) {
	if options.Event != nil {
		options.Event(level, message)
	}
}
