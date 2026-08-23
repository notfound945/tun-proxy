package app

import (
	"os"
	"sync"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"

	"github.com/hailinpan/tun-proxy/internal/apperror"
)

type runMonitor struct {
	mutex   sync.RWMutex
	started time.Time
	digest  string
	reload  runtimestatus.ReloadStats
	network runtimestatus.NetworkStats
	limits  runtimestatus.Limits
	ipv6    runtimestatus.IPv6Status
}

func (monitor *runMonitor) networkResult(at time.Time, err error) {
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	monitor.network.LastAttempt = at
	if err != nil {
		monitor.network.Failures++
		monitor.network.LastError = err.Error()
		return
	}
	monitor.network.Refreshes++
	monitor.network.LastSuccess = at
	monitor.network.LastError = ""
}

func newRunMonitor(started time.Time, digest string, runtime *config.Config, ipv6Enabled bool, fallbackReason string) *runMonitor {
	return &runMonitor{
		started: started, digest: digest, limits: limitsFromConfig(runtime),
		ipv6: runtimestatus.IPv6Status{Configured: runtime.FakeIPv6 != nil, Enabled: ipv6Enabled, FallbackReason: fallbackReason},
	}
}

func (monitor *runMonitor) reloadResult(at time.Time, requestID, digest string, runtime *config.Config, err error) {
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	monitor.reload.LastRequestID = requestID
	monitor.reload.LastAttempt = at
	monitor.reload.LastCompleted = at
	if err != nil {
		monitor.reload.Failures++
		monitor.reload.LastResult = "failed"
		monitor.reload.LastErrorCode = string(apperror.CodeOf(err))
		monitor.reload.LastError = err.Error()
		return
	}
	monitor.reload.Successes++
	monitor.reload.LastResult = "succeeded"
	monitor.reload.LastSuccess = at
	monitor.reload.LastErrorCode = ""
	monitor.reload.LastError = ""
	monitor.digest = digest
	monitor.limits = limitsFromConfig(runtime)
}

func (monitor *runMonitor) snapshot() runtimestatus.Snapshot {
	monitor.mutex.RLock()
	defer monitor.mutex.RUnlock()
	return runtimestatus.Snapshot{
		PID: os.Getpid(), StartedAt: monitor.started, ConfigDigest: monitor.digest,
		Reload: monitor.reload, Network: monitor.network, Limits: monitor.limits, IPv6: monitor.ipv6,
	}
}

func limitsFromConfig(runtime *config.Config) runtimestatus.Limits {
	limits := runtimestatus.Limits{
		TCPFlows: runtime.Sessions.MaxTCPFlows, UDPSessions: runtime.Sessions.MaxUDPSessions,
		UDPSessionsPerSource: runtime.Sessions.MaxUDPSessionsPerSource,
		DNSConcurrent:        runtime.DNS.MaxConcurrent, FakeIPMappings: runtime.FakeIP.MaxMappings,
		PacketQueue: runtime.TUN.PacketQueue, PacketBuffers: runtime.TUN.BufferPool,
	}
	if runtime.FakeIPv6 != nil {
		limits.FakeIPv6Mappings = runtime.FakeIPv6.MaxMappings
	}
	return limits
}
