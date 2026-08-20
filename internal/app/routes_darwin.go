//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hailinpan/tun-proxy/internal/system"
)

// removeRecordedRoutes rolls back additional routes in reverse order before
// the legacy IPv4 route. Progress is made durable after every successful
// deletion so crash cleanup can safely resume.
func removeRecordedRoutes(ctx context.Context, runner system.CommandRunner, statePath string, state *system.State) error {
	var failures []error
	for index := len(state.Routes) - 1; index >= 0; index-- {
		route := state.Routes[index]
		if err := system.RemoveRoute(ctx, runner, route); err != nil {
			failures = append(failures, fmt.Errorf("remove recorded route %s: %w", route.Prefix, err))
			continue
		}
		state.Routes = append(state.Routes[:index], state.Routes[index+1:]...)
		if err := system.WriteState(statePath, *state); err != nil {
			failures = append(failures, fmt.Errorf("persist route rollback after removing %s: %w", route.Prefix, err))
		}
	}
	if state.Route != nil {
		route := *state.Route
		if err := system.RemoveRoute(ctx, runner, route); err != nil {
			failures = append(failures, fmt.Errorf("remove Fake IP route %s: %w", route.Prefix, err))
		} else {
			state.Route = nil
			if err := system.WriteState(statePath, *state); err != nil {
				failures = append(failures, fmt.Errorf("persist route rollback after removing %s: %w", route.Prefix, err))
			}
		}
	}
	return errors.Join(failures...)
}
