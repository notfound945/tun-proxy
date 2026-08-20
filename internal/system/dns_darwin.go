//go:build darwin

package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

const networkSetup = "/usr/sbin/networksetup"

func SnapshotDNS(ctx context.Context, runner CommandRunner) ([]DNSState, error) {
	output, err := runner.Run(ctx, networkSetup, "-listnetworkserviceorder")
	if err != nil {
		return nil, fmt.Errorf("list network service order: %w", err)
	}
	services := activeNetworkServices(parseNetworkServiceOrder(string(output)))
	if len(services) == 0 {
		return nil, errors.New("no active IPv4 network services found")
	}
	return snapshotDNSStates(ctx, runner, services)
}

func snapshotDNSStates(ctx context.Context, runner CommandRunner, services []networkService) ([]DNSState, error) {
	states := make([]DNSState, 0, len(services))
	for _, service := range services {
		servers, err := getDNS(ctx, runner, service.Name)
		if err != nil {
			return nil, err
		}
		states = append(states, DNSState{Service: service.Name, Previous: servers})
	}
	return states, nil
}

func ApplyDNS(ctx context.Context, runner CommandRunner, states []DNSState, loopback netip.Addr) ([]DNSState, error) {
	planned, err := PlanDNS(states, loopback)
	if err != nil {
		return nil, err
	}
	applied := make([]DNSState, 0, len(planned))
	for _, state := range planned {
		if err := setDNS(ctx, runner, state.Service, state.Applied); err != nil {
			return applied, err
		}
		applied = append(applied, state)
	}
	return applied, nil
}

// PlanDNS fills the exact replacement values without performing I/O, allowing
// callers to persist a complete recovery plan before the first mutation.
func PlanDNS(states []DNSState, loopback netip.Addr) ([]DNSState, error) {
	if !loopback.IsValid() || !loopback.IsLoopback() {
		return nil, fmt.Errorf("DNS replacement %s is not loopback", loopback)
	}
	planned := make([]DNSState, 0, len(states))
	for _, state := range states {
		state.Applied = []string{loopback.String()}
		planned = append(planned, state)
	}
	return planned, nil
}

// RestoreDNS restores entries that still match Applied. Entries changed by a
// user or another process are returned in remaining and are never overwritten.
func RestoreDNS(ctx context.Context, runner CommandRunner, states []DNSState) (remaining []DNSState, err error) {
	var failures []error
	for _, state := range states {
		current, getErr := getDNS(ctx, runner, state.Service)
		if getErr != nil {
			remaining = append(remaining, state)
			failures = append(failures, getErr)
			continue
		}
		if slices.Equal(current, state.Previous) {
			continue
		}
		if !slices.Equal(current, state.Applied) {
			remaining = append(remaining, state)
			failures = append(failures, fmt.Errorf("refuse to restore DNS service %q: current servers %v do not match applied servers %v", state.Service, current, state.Applied))
			continue
		}
		if setErr := setDNS(ctx, runner, state.Service, state.Previous); setErr != nil {
			remaining = append(remaining, state)
			failures = append(failures, setErr)
		}
	}
	return remaining, errors.Join(failures...)
}

func getDNS(ctx context.Context, runner CommandRunner, service string) ([]string, error) {
	output, err := runner.Run(ctx, networkSetup, "-getdnsservers", service)
	if err != nil {
		return nil, fmt.Errorf("get DNS for service %q: %w", service, err)
	}
	trimmed := strings.TrimSpace(string(output))
	if strings.HasPrefix(trimmed, "There aren't any DNS Servers set on") {
		return nil, nil
	}
	var servers []string
	scanner := bufio.NewScanner(strings.NewReader(trimmed))
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		address, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("service %q returned invalid DNS address %q", service, value)
		}
		servers = append(servers, address.String())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read DNS servers for service %q: %w", service, err)
	}
	return servers, nil
}

func setDNS(ctx context.Context, runner CommandRunner, service string, servers []string) error {
	args := []string{"-setdnsservers", service}
	if len(servers) == 0 {
		args = append(args, "Empty")
	} else {
		for _, server := range servers {
			if _, err := netip.ParseAddr(server); err != nil {
				return fmt.Errorf("refuse invalid DNS server %q for service %q", server, service)
			}
			args = append(args, server)
		}
	}
	if _, err := runner.Run(ctx, networkSetup, args...); err != nil {
		return fmt.Errorf("set DNS for service %q: %w", service, err)
	}
	return nil
}

type networkService struct {
	Name   string
	Device string
}

func parseNetworkServiceOrder(output string) []networkService {
	var services []networkService
	var pendingName string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "An asterisk") {
			continue
		}
		if strings.HasPrefix(line, "(Hardware Port:") {
			if pendingName == "" {
				continue
			}
			marker := "Device:"
			position := strings.LastIndex(line, marker)
			if position < 0 {
				pendingName = ""
				continue
			}
			device := strings.TrimSpace(strings.TrimSuffix(line[position+len(marker):], ")"))
			if device != "" {
				services = append(services, networkService{Name: pendingName, Device: device})
			}
			pendingName = ""
			continue
		}
		if strings.HasPrefix(line, "(") && !strings.HasPrefix(line, "(*)") {
			if end := strings.Index(line, ") "); end >= 0 {
				pendingName = strings.TrimSpace(line[end+2:])
			}
			continue
		}
	}
	return services
}

func activeNetworkServices(services []networkService) []networkService {
	active := make([]networkService, 0, len(services))
	for _, service := range services {
		iface, err := net.InterfaceByName(service.Device)
		if err != nil || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		hasIPv4 := false
		for _, rawAddress := range addresses {
			address, _, err := net.ParseCIDR(rawAddress.String())
			if err == nil && address.To4() != nil && !address.IsLoopback() {
				hasIPv4 = true
				break
			}
		}
		if hasIPv4 {
			active = append(active, service)
		}
	}
	return active
}
