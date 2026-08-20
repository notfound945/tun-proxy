//go:build darwin

package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

var ErrRouteNotFound = errors.New("route not found")

type RouteLookup struct {
	Destination string
	Mask        string
	Interface   string
	Gateway     string
}

func LookupRoute(ctx context.Context, address netip.Addr) (RouteLookup, error) {
	return lookupRoute(ctx, NativeCommandRunner{}, address)
}

// LookupDefaultRoute queries the family default directly so cloned host
// routes for a previously contacted probe address cannot hide it.
func LookupDefaultRoute(ctx context.Context, ipv6 bool) (RouteLookup, error) {
	output, err := NativeCommandRunner{}.Run(ctx, "/sbin/route", routeDefaultLookupArgs(ipv6)...)
	return parseRouteCommand(output, err)
}

// LookupRouteScoped resolves address using the routing scope of one physical
// interface. It is used before default-route capture to prove an independent
// egress path for every configured outbound.
func LookupRouteScoped(ctx context.Context, address netip.Addr, interfaceName string) (RouteLookup, error) {
	return lookupRouteScoped(ctx, NativeCommandRunner{}, address, interfaceName)
}

// LookupDefaultRouteScoped returns one interface's family-specific default
// gateway without being affected by more-specific host bypass routes.
func LookupDefaultRouteScoped(ctx context.Context, ipv6 bool, interfaceName string) (RouteLookup, error) {
	if !validSystemInterface(interfaceName) {
		return RouteLookup{}, fmt.Errorf("invalid route interface %q", interfaceName)
	}
	output, err := NativeCommandRunner{}.Run(ctx, "/sbin/route", routeDefaultLookupScopedArgs(ipv6, interfaceName)...)
	return parseRouteCommand(output, err)
}

func lookupRoute(ctx context.Context, runner CommandRunner, address netip.Addr) (RouteLookup, error) {
	if !address.IsValid() || address.Is4In6() {
		return RouteLookup{}, fmt.Errorf("invalid route lookup address %s", address)
	}
	output, err := runner.Run(ctx, "/sbin/route", routeLookupArgs(address)...)
	return parseRouteCommand(output, err)
}

func lookupRouteScoped(ctx context.Context, runner CommandRunner, address netip.Addr, interfaceName string) (RouteLookup, error) {
	if !address.IsValid() || address.Is4In6() {
		return RouteLookup{}, fmt.Errorf("invalid scoped route lookup address %s", address)
	}
	if !validSystemInterface(interfaceName) {
		return RouteLookup{}, fmt.Errorf("invalid route interface %q", interfaceName)
	}
	output, err := runner.Run(ctx, "/sbin/route", routeLookupScopedArgs(address, interfaceName)...)
	return parseRouteCommand(output, err)
}

func parseRouteCommand(output []byte, commandErr error) (RouteLookup, error) {
	text := string(output)
	if strings.Contains(text, "not in table") || (commandErr != nil && strings.Contains(commandErr.Error(), "not in table")) {
		return RouteLookup{}, ErrRouteNotFound
	}
	if commandErr != nil {
		return RouteLookup{}, commandErr
	}
	return ParseRouteLookup(text)
}

func ParseRouteLookup(output string) (RouteLookup, error) {
	lookup := RouteLookup{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "destination":
			lookup.Destination = strings.TrimSpace(value)
		case "mask":
			lookup.Mask = strings.TrimSpace(value)
		case "interface":
			lookup.Interface = strings.TrimSpace(value)
		case "gateway":
			lookup.Gateway = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return RouteLookup{}, fmt.Errorf("parse route output: %w", err)
	}
	if lookup.Destination == "" || lookup.Interface == "" {
		return RouteLookup{}, fmt.Errorf("route output lacks destination or interface")
	}
	return lookup, nil
}

// CheckPrefixAvailable accepts ordinary default routing but rejects an
// existing specific route for the configured Fake IP space.
func CheckPrefixAvailable(ctx context.Context, prefix netip.Prefix) error {
	return checkPrefixAvailable(ctx, NativeCommandRunner{}, prefix)
}

func checkPrefixAvailable(ctx context.Context, runner CommandRunner, prefix netip.Prefix) error {
	lookup, err := lookupRoute(ctx, runner, firstUsable(prefix))
	if err != nil {
		// macOS reports exit status 68 and "not in table" when an address
		// family has no route at all. That is an available prefix, not a
		// conflict. Other lookup failures remain fatal.
		if errors.Is(err, ErrRouteNotFound) {
			return nil
		}
		return fmt.Errorf("check Fake IP route: %w", err)
	}
	if defaultRouteLookup(lookup) {
		return nil
	}
	return fmt.Errorf("Fake IP prefix %s conflicts with route destination %s on %s", prefix, lookup.Destination, lookup.Interface)
}

func defaultRouteDestination(destination string) bool {
	return destination == "default" || destination == "0.0.0.0" || destination == "::"
}

// macOS renders 0.0.0.0/1 and ::/1 with destination "default" too, but with
// a non-default mask. Both fields are required to distinguish a real default
// route from a split-default capture route.
func defaultRouteLookup(lookup RouteLookup) bool {
	if !defaultRouteDestination(lookup.Destination) {
		return false
	}
	switch lookup.Mask {
	case "", "default", "0.0.0.0", "::", "0.0.0.0/0", "::/0":
		return true
	default:
		return false
	}
}

func AddRoute(ctx context.Context, runner CommandRunner, route RouteState) error {
	prefix, gateway, err := validateRouteState(route)
	if err != nil {
		return err
	}
	args := routeStateMutationArgs("add", prefix, route.Interface, gateway, route.Scope)
	if _, err := runner.Run(ctx, "/sbin/route", args...); err != nil {
		return fmt.Errorf("add route %s via %s: %w", prefix, route.Interface, err)
	}
	return nil
}

func VerifyRoute(ctx context.Context, runner CommandRunner, route RouteState) error {
	prefix, gateway, err := validateRouteState(route)
	if err != nil {
		return err
	}
	output, commandErr := runner.Run(ctx, "/sbin/route", routeStateGetArgs(prefix, route.Scope)...)
	lookup, err := parseRouteCommand(output, commandErr)
	if err != nil {
		return fmt.Errorf("verify route %s: %w", prefix, err)
	}
	if defaultRouteLookup(lookup) || lookup.Interface != route.Interface {
		return fmt.Errorf("route %s resolves via destination %q interface %q, want interface %q", prefix, lookup.Destination, lookup.Interface, route.Interface)
	}
	if prefix.Bits() == 1 && !splitRouteMaskMatches(prefix, lookup.Mask) {
		return fmt.Errorf("route %s resolves via mask %q, want /1", prefix, lookup.Mask)
	}
	if gateway.IsValid() && lookup.Gateway != route.Gateway {
		return fmt.Errorf("route %s resolves via gateway %q, want %q", prefix, lookup.Gateway, route.Gateway)
	}
	return nil
}

// RemoveRoute is idempotent when the recorded route is already absent. It
// refuses to delete when the selected destination now points to another
// interface.
func RemoveRoute(ctx context.Context, runner CommandRunner, route RouteState) error {
	prefix, gateway, err := validateRouteState(route)
	if err != nil {
		return err
	}
	output, commandErr := runner.Run(ctx, "/sbin/route", routeStateGetArgs(prefix, route.Scope)...)
	lookup, err := parseRouteCommand(output, commandErr)
	if errors.Is(err, ErrRouteNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify route %s: %w", prefix, err)
	}
	if defaultRouteLookup(lookup) {
		return nil
	}
	if lookup.Interface != route.Interface {
		exists, interfaceErr := routeInterfaceExists(ctx, runner, route.Interface)
		if interfaceErr != nil {
			return fmt.Errorf("verify recorded route interface %s: %w", route.Interface, interfaceErr)
		}
		if !exists {
			// Closing a utun file descriptor, or detaching a physical
			// interface, makes macOS remove its routes before crash cleanup
			// runs. A lookup can then select an equally specific route owned
			// by another interface. The recorded route is already absent and
			// the replacement must not be deleted.
			return nil
		}
		return fmt.Errorf("refuse to remove route %s: current interface %q does not match recorded interface %q", prefix, lookup.Interface, route.Interface)
	}
	if prefix.Bits() == 1 && !splitRouteMaskMatches(prefix, lookup.Mask) {
		return fmt.Errorf("refuse to remove route %s: current mask %q does not match /1", prefix, lookup.Mask)
	}
	if gateway.IsValid() && lookup.Gateway != route.Gateway {
		return fmt.Errorf("refuse to remove route %s: current gateway %q does not match recorded gateway %q", prefix, lookup.Gateway, route.Gateway)
	}
	if _, err := runner.Run(ctx, "/sbin/route", routeStateMutationArgs("delete", prefix, route.Interface, gateway, route.Scope)...); err != nil {
		return fmt.Errorf("delete route %s via %s: %w", prefix, route.Interface, err)
	}
	return nil
}

func routeInterfaceExists(ctx context.Context, runner CommandRunner, interfaceName string) (bool, error) {
	_, err := runner.Run(ctx, "/sbin/ifconfig", interfaceName)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "interface "+interfaceName+" does not exist") {
		return false, nil
	}
	return false, err
}

func routeGetArgs(prefix netip.Prefix) []string {
	return routeLookupArgs(firstUsable(prefix))
}

func routeStateGetArgs(prefix netip.Prefix, scope string) []string {
	if scope != "" {
		return routeLookupScopedArgs(firstUsable(prefix), scope)
	}
	return routeGetArgs(prefix)
}

func routeLookupArgs(address netip.Addr) []string {
	args := []string{"-n", "get"}
	if address.Is6() {
		args = append(args, "-inet6")
	}
	return append(args, address.String())
}

func routeLookupScopedArgs(address netip.Addr, interfaceName string) []string {
	args := []string{"-n", "get"}
	if address.Is6() {
		args = append(args, "-inet6")
	}
	return append(args, "-ifscope", interfaceName, address.String())
}

func routeDefaultLookupScopedArgs(ipv6 bool, interfaceName string) []string {
	args := []string{"-n", "get"}
	if ipv6 {
		args = append(args, "-inet6")
	}
	return append(args, "-ifscope", interfaceName, "default")
}

func routeDefaultLookupArgs(ipv6 bool) []string {
	args := []string{"-n", "get"}
	if ipv6 {
		args = append(args, "-inet6")
	}
	return append(args, "default")
}

func routeMutationArgs(operation string, prefix netip.Prefix, interfaceName string) []string {
	return routeStateMutationArgs(operation, prefix, interfaceName, netip.Addr{}, "")
}

func routeStateMutationArgs(operation string, prefix netip.Prefix, interfaceName string, gateway netip.Addr, scope string) []string {
	args := []string{"-n", operation}
	if prefix.Addr().Is6() {
		args = append(args, "-inet6")
	}
	bits := 128
	if prefix.Addr().Is4() {
		bits = 32
	}
	if gateway.IsValid() {
		kind := "-net"
		destination := prefix.Masked().String()
		if prefix.Bits() == bits {
			kind = "-host"
			destination = prefix.Addr().String()
		}
		args = append(args, kind)
		if scope != "" {
			args = append(args, "-ifscope", scope)
		}
		return append(args, destination, gateway.String())
	}
	if scope != "" {
		args = append(args, "-ifscope", scope)
	}
	return append(args, "-net", prefix.Masked().String(), "-interface", interfaceName)
}

func validateRouteState(route RouteState) (netip.Prefix, netip.Addr, error) {
	prefix, err := netip.ParsePrefix(route.Prefix)
	if err != nil || prefix.Addr().Is4In6() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("invalid route prefix %q", route.Prefix)
	}
	if !validSystemInterface(route.Interface) {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("invalid route interface %q", route.Interface)
	}
	if route.Scope != "" && (!validSystemInterface(route.Scope) || route.Scope != route.Interface) {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("invalid route scope %q for interface %q", route.Scope, route.Interface)
	}
	if route.Gateway == "" {
		if route.Scope != "" {
			return netip.Prefix{}, netip.Addr{}, fmt.Errorf("route scope %q requires a gateway", route.Scope)
		}
		return prefix, netip.Addr{}, nil
	}
	gateway, err := netip.ParseAddr(route.Gateway)
	if err != nil || gateway.Is4In6() || gateway.Is4() != prefix.Addr().Is4() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("invalid route gateway %q for prefix %s", route.Gateway, prefix)
	}
	return prefix, gateway, nil
}

func splitRouteMaskMatches(prefix netip.Prefix, mask string) bool {
	if prefix.Addr().Is4() {
		return mask == "128.0.0.0"
	}
	return mask == "8000::"
}

func firstUsable(prefix netip.Prefix) netip.Addr {
	address := prefix.Masked().Addr()
	for range 3 {
		next := address.Next()
		if !next.IsValid() || !prefix.Contains(next) {
			break
		}
		address = next
		if !address.IsUnspecified() && !address.IsLoopback() {
			return address
		}
	}
	return address
}

func validSystemInterface(value string) bool {
	if value == "" || len(value) > 15 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
