//go:build darwin

// Package app orchestrates validation and application lifecycle without
// implementing protocol details.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/hailinpan/tun-proxy/internal/apperror"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/interfaceinfo"
	"github.com/hailinpan/tun-proxy/internal/system"
)

func Preflight(ctx context.Context, runtime *config.Config) error {
	return preflight(ctx, runtime, pathOwnership{
		systemUID:      os.Geteuid(),
		persistenceUID: os.Geteuid(),
	})
}

// PreflightManaged validates host prerequisites for the split-privilege
// LaunchDaemon. Recovery state remains root-owned while Fake IP persistence is
// owned by the dedicated non-root worker.
func PreflightManaged(ctx context.Context, runtime *config.Config, workerUID uint32) error {
	if workerUID == 0 {
		return errors.New("managed service worker UID must be non-root")
	}
	return preflight(ctx, runtime, pathOwnership{
		systemUID:      0,
		persistenceUID: int(workerUID),
	})
}

type pathOwnership struct {
	systemUID      int
	persistenceUID int
}

func preflight(ctx context.Context, runtime *config.Config, ownership pathOwnership) error {
	var failures []error
	discovered, discoveryErr := discoverInterfaceDNS(ctx, runtime, system.NativeCommandRunner{})
	if discoveryErr != nil {
		failures = append(failures, discoveryErr)
	}
	effectiveRuntime := runtimeWithInterfaceDNS(runtime, discovered)
	if err := system.RequireRoot(); err != nil {
		failures = append(failures, err)
	}
	if err := checkPaths(runtime, ownership); err != nil {
		failures = append(failures, err)
	}
	if err := checkInterfaces(runtime); err != nil {
		failures = append(failures, err)
	}
	if err := system.CheckPrefixAvailable(ctx, runtime.FakeIP.Prefix); err != nil {
		failures = append(failures, err)
	}
	ipv6Enabled, _ := IPv6DataPathAvailable(ctx, runtime)
	if ipv6Enabled {
		if err := system.CheckPrefixAvailable(ctx, runtime.FakeIPv6.Prefix); err != nil {
			failures = append(failures, err)
		}
	}
	if _, err := planDefaultRouteCapture(ctx, effectiveRuntime, ipv6Enabled, system.LookupRouteScoped, system.LookupDefaultRouteScoped); err != nil {
		failures = append(failures, err)
	}
	if err := checkDNSListeners(runtime); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func PreflightReload(_ context.Context, current, next *config.Config) error {
	if err := config.ValidateReload(current, next); err != nil {
		return apperror.Wrap(apperror.CodeConfigRestartRequired, "service.reload", "configuration changes require a service restart", err)
	}
	if err := checkInterfaces(next); err != nil {
		return apperror.Wrap(apperror.CodeReloadRejected, "service.reload", "reload prerequisites are not satisfied", err)
	}
	return nil
}

func checkPaths(runtime *config.Config, ownership pathOwnership) error {
	paths := map[string]struct {
		path string
		uid  int
	}{
		"system.state_file":        {path: runtime.System.StateFile, uid: ownership.systemUID},
		"system.lock_file":         {path: runtime.System.LockFile, uid: ownership.systemUID},
		"fake_ip.persistence_file": {path: runtime.FakeIP.PersistenceFile, uid: ownership.persistenceUID},
	}
	if runtime.FakeIPv6 != nil {
		paths["fake_ipv6.persistence_file"] = struct {
			path string
			uid  int
		}{path: runtime.FakeIPv6.PersistenceFile, uid: ownership.persistenceUID}
	}
	var failures []error
	for name, expected := range paths {
		path := expected.path
		parent := filepath.Dir(path)
		info, err := os.Stat(parent)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s parent %q: %w", name, parent, err))
			continue
		}
		if !info.IsDir() {
			failures = append(failures, fmt.Errorf("%s parent %q is not a directory", name, parent))
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != expected.uid {
			failures = append(failures, fmt.Errorf("%s parent %q is not owned by UID %d", name, parent, expected.uid))
		}
		if info.Mode().Perm()&0o022 != 0 {
			failures = append(failures, fmt.Errorf("%s parent %q is writable by group or others", name, parent))
		}
		if target, err := os.Lstat(path); err == nil && (target.Mode()&os.ModeSymlink != 0 || !target.Mode().IsRegular()) {
			failures = append(failures, fmt.Errorf("%s %q is not a regular file", name, path))
		} else if err == nil && name == "system.state_file" {
			failures = append(failures, fmt.Errorf("%s %q already exists; inspect status and run cleanup before starting", name, path))
		} else if err == nil {
			if stat, ok := target.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != expected.uid {
				failures = append(failures, fmt.Errorf("%s %q is not owned by UID %d", name, path, expected.uid))
			}
			if target.Mode().Perm()&0o077 != 0 {
				failures = append(failures, fmt.Errorf("%s %q permissions are %04o, want 0600 or stricter", name, path, target.Mode().Perm()))
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("inspect %s %q: %w", name, path, err))
		}
	}
	if statusPath := runtime.System.StateFile + ".sock"; len(statusPath) > 103 {
		failures = append(failures, fmt.Errorf("runtime status socket path %q exceeds the macOS limit", statusPath))
	}
	return errors.Join(failures...)
}

func checkInterfaces(runtime *config.Config) error {
	seen := make(map[string]struct{})
	var failures []error
	for _, outbound := range runtime.Outbounds {
		if outbound.Type != "direct" {
			continue
		}
		if _, exists := seen[outbound.Interface]; exists {
			continue
		}
		seen[outbound.Interface] = struct{}{}
		iface, err := interfaceinfo.Get(outbound.Interface)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !iface.Up() {
			failures = append(failures, fmt.Errorf("interface %q is down", iface.Name))
			continue
		}
		hasIPv4 := false
		for _, rawAddress := range iface.Addresses {
			address, _, err := net.ParseCIDR(rawAddress)
			if err == nil && address.To4() != nil && !address.IsLoopback() {
				hasIPv4 = true
				break
			}
		}
		if !hasIPv4 {
			failures = append(failures, fmt.Errorf("interface %q has no usable IPv4 address", iface.Name))
		}
	}
	return errors.Join(failures...)
}

func checkDNSListeners(runtime *config.Config) error {
	address := runtime.DNS.Listen.String()
	var failures []error
	if runtime.DNS.UDP {
		listener, err := net.ListenPacket("udp4", address)
		if err != nil {
			failures = append(failures, fmt.Errorf("dns.listen UDP %s is unavailable: %w", address, err))
		} else {
			_ = listener.Close()
		}
	}
	if runtime.DNS.TCP {
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			failures = append(failures, fmt.Errorf("dns.listen TCP %s is unavailable: %w", address, err))
		} else {
			_ = listener.Close()
		}
	}
	return errors.Join(failures...)
}

func Summary(runtime *config.Config) string {
	interfaces := make(map[string]struct{})
	for _, outbound := range runtime.Outbounds {
		if outbound.Interface != "" {
			interfaces[outbound.Interface] = struct{}{}
		}
	}
	fakeIPv6 := "disabled"
	if runtime.FakeIPv6 != nil {
		fakeIPv6 = runtime.FakeIPv6.Prefix.String() + " (runtime-gated)"
	}
	return fmt.Sprintf("config valid: fake_ip=%s fake_ipv6=%s default_route=%t dns=%s outbounds=%d rules=%d interfaces=%s",
		runtime.FakeIP.Prefix,
		fakeIPv6,
		runtime.Capture.DefaultRoute,
		runtime.DNS.Listen,
		len(runtime.Outbounds),
		len(runtime.Rules),
		strings.Join(sortedKeys(interfaces), ","),
	)
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
