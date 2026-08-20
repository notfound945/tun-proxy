package procattrib

import "net/netip"

type socketRecord struct {
	protocol   Protocol
	local      netip.AddrPort
	remote     netip.AddrPort
	remoteOpen bool
	socketID   uint64
	generation uint64
}

func matches(flow Flow, socket socketRecord) bool {
	if flow.Protocol != socket.protocol {
		return false
	}
	if !endpointMatches(flow.Source, socket.local, true) {
		return false
	}
	if socket.remoteOpen {
		return flow.Protocol == UDP
	}
	return endpointMatches(flow.Destination, socket.remote, false)
}

func endpointMatches(flow, socket netip.AddrPort, allowWildcard bool) bool {
	if flow.Port() != socket.Port() {
		return false
	}
	flowAddress := flow.Addr().Unmap()
	socketAddress := socket.Addr().Unmap()
	if allowWildcard && socketAddress.IsUnspecified() {
		return flowAddress.Is4() == socketAddress.Is4()
	}
	return flowAddress == socketAddress
}
