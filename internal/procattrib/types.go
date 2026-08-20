// Package procattrib contains the macOS process-attribution capability spike.
// It is intentionally not connected to the production rules engine: callers
// must treat anything other than one unique owner as indeterminate.
package procattrib

import (
	"fmt"
	"net/netip"
	"time"
)

type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

type Flow struct {
	Protocol    Protocol
	Source      netip.AddrPort
	Destination netip.AddrPort
}

func (flow Flow) Validate() error {
	if flow.Protocol != TCP && flow.Protocol != UDP {
		return fmt.Errorf("unsupported protocol %q", flow.Protocol)
	}
	if !flow.Source.IsValid() || !flow.Destination.IsValid() {
		return fmt.Errorf("flow endpoints must be valid: source=%s destination=%s", flow.Source, flow.Destination)
	}
	if flow.Source.Port() == 0 || flow.Destination.Port() == 0 {
		return fmt.Errorf("flow endpoint ports must be non-zero: source=%s destination=%s", flow.Source, flow.Destination)
	}
	if flow.Source.Addr().Unmap().Is4() != flow.Destination.Addr().Unmap().Is4() {
		return fmt.Errorf("flow endpoints must use one address family: source=%s destination=%s", flow.Source, flow.Destination)
	}
	return nil
}

type Owner struct {
	PID        int    `json:"pid"`
	FD         int    `json:"fd"`
	Name       string `json:"name"`
	SocketID   uint64 `json:"socket_id"`
	Generation uint64 `json:"generation"`
}

type Outcome string

const (
	OutcomeNone      Outcome = "none"
	OutcomeUnique    Outcome = "unique"
	OutcomeAmbiguous Outcome = "ambiguous"
)

type Diagnostics struct {
	Duration          time.Duration `json:"duration"`
	PIDsScanned       int           `json:"pids_scanned"`
	SocketFDsScanned  int           `json:"socket_fds_scanned"`
	PermissionDenied  int           `json:"permission_denied"`
	ProcessesVanished int           `json:"processes_vanished"`
}

type Result struct {
	Flow        Flow        `json:"flow"`
	Outcome     Outcome     `json:"outcome"`
	Owners      []Owner     `json:"owners"`
	Diagnostics Diagnostics `json:"diagnostics"`
}

func outcomeForOwners(owners []Owner) Outcome {
	switch len(owners) {
	case 0:
		return OutcomeNone
	case 1:
		return OutcomeUnique
	default:
		return OutcomeAmbiguous
	}
}
