//go:build darwin

package app

import (
	"context"
	"fmt"

	"github.com/hailinpan/tun-proxy/internal/system"
)

// removeRecordedRoutes rolls back additional routes in reverse order before
// the legacy IPv4 route. Progress is made durable after every successful
// deletion so crash cleanup can safely resume.
func removeRecordedRoutes(ctx context.Context, runner system.CommandRunner, statePath string, state *system.State) error {
	for index := len(state.Routes) - 1; index >= 0; index-- {
		route := state.Routes[index]
		if err := system.RemoveRoute(ctx, runner, route); err != nil {
			return fmt.Errorf("remove recorded route %s: %w", route.Prefix, err)
		}
		state.Routes = state.Routes[:index]
		if err := system.WriteState(statePath, *state); err != nil {
			return fmt.Errorf("persist route rollback: %w", err)
		}
	}
	if state.Route == nil {
		return nil
	}
	if err := system.RemoveRoute(ctx, runner, *state.Route); err != nil {
		return fmt.Errorf("remove Fake IP route %s: %w", state.Route.Prefix, err)
	}
	state.Route = nil
	if err := system.WriteState(statePath, *state); err != nil {
		return fmt.Errorf("persist route rollback: %w", err)
	}
	return nil
}
