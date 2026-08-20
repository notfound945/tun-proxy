// Package netstack isolates all gVisor types and packet ownership rules from
// the rest of tun-proxy.
package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const nicID tcpip.NICID = 1

// Config contains bounded resource settings for a Stack.
type Config struct {
	MTU         int
	PacketQueue int
	MaxTCPFlows int
}

// TCPFlow is immutable metadata captured from the first packet of a TCP flow.
// It intentionally contains only standard-library types.
type TCPFlow struct {
	SourceIP        netip.Addr
	SourcePort      uint16
	DestinationIP   netip.Addr
	DestinationPort uint16
}

// TCPHandler owns conn for the duration of the call. The Stack closes conn
// after the handler returns or when the Stack is closed.
type TCPHandler func(ctx context.Context, flow TCPFlow, conn net.Conn) error

// UDPFlow identifies one connected UDP session.
type UDPFlow struct {
	SourceIP        netip.Addr
	SourcePort      uint16
	DestinationIP   netip.Addr
	DestinationPort uint16
}

type UDPHandler func(ctx context.Context, flow UDPFlow, conn net.Conn) error

// Stats is a point-in-time snapshot of stack activity.
type Stats struct {
	InjectedPackets     uint64
	InjectedBytes       uint64
	EmittedPackets      uint64
	EmittedBytes        uint64
	ActiveTCPFlows      uint64
	TotalTCPFlows       uint64
	RejectedTCPFlows    uint64
	TCPHandlerErrors    uint64
	ActiveUDPSessions   uint64
	TotalUDPSessions    uint64
	RejectedUDPSessions uint64
	UDPHandlerErrors    uint64
}

type counters struct {
	injectedPackets     atomic.Uint64
	injectedBytes       atomic.Uint64
	emittedPackets      atomic.Uint64
	emittedBytes        atomic.Uint64
	activeTCPFlows      atomic.Uint64
	totalTCPFlows       atomic.Uint64
	rejectedTCPFlows    atomic.Uint64
	tcpHandlerErrors    atomic.Uint64
	activeUDPSessions   atomic.Uint64
	totalUDPSessions    atomic.Uint64
	rejectedUDPSessions atomic.Uint64
	udpHandlerErrors    atomic.Uint64
}

// Stack terminates IPv4/IPv6 TCP and UDP flows and exchanges raw IP packets with a TUN
// adapter. Methods are safe for concurrent use.
type Stack struct {
	stack      *stack.Stack
	link       *channel.Endpoint
	handler    TCPHandler
	udpHandler UDPHandler
	ctx        context.Context
	cancel     context.CancelFunc
	closeOnce  sync.Once
	flows      sync.WaitGroup
	tcpSlots   chan struct{}
	metrics    counters
}

func New(config Config, handler TCPHandler, udpHandler UDPHandler) (*Stack, error) {
	if config.MTU < header.IPv4MinimumSize {
		return nil, fmt.Errorf("netstack MTU must be at least %d, got %d", header.IPv4MinimumSize, config.MTU)
	}
	if config.PacketQueue <= 0 {
		return nil, errors.New("netstack packet queue must be positive")
	}
	if config.MaxTCPFlows <= 0 {
		return nil, errors.New("netstack maximum TCP flow count must be positive")
	}
	if handler == nil {
		return nil, errors.New("netstack TCP handler is required")
	}
	if udpHandler == nil {
		return nil, errors.New("netstack UDP handler is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := &Stack{
		handler: handler, udpHandler: udpHandler, ctx: ctx, cancel: cancel,
		tcpSlots: make(chan struct{}, config.MaxTCPFlows),
	}
	result.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	result.link = channel.New(config.PacketQueue, uint32(config.MTU), "")
	if err := result.stack.CreateNIC(nicID, result.link); err != nil {
		result.closeResources()
		return nil, fmt.Errorf("create netstack NIC: %s", err)
	}
	if err := result.stack.SetPromiscuousMode(nicID, true); err != nil {
		result.closeResources()
		return nil, fmt.Errorf("enable netstack promiscuous mode: %s", err)
	}
	if err := result.stack.SetSpoofing(nicID, true); err != nil {
		result.closeResources()
		return nil, fmt.Errorf("enable netstack spoofing: %s", err)
	}
	result.stack.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	forwarder := tcp.NewForwarder(result.stack, 0, config.MaxTCPFlows, result.handleTCP)
	result.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.HandlePacket)
	udpForwarder := udp.NewForwarder(result.stack, result.handleUDP)
	result.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)
	return result, nil
}

// InjectIPv4 copies one complete IPv4 packet into gVisor. The caller may reuse
// packet as soon as this method returns.
func (s *Stack) InjectIPv4(packet []byte) error {
	if len(packet) < header.IPv4MinimumSize || packet[0]>>4 != 4 {
		return errors.New("netstack input is not a complete IPv4 packet")
	}
	return s.inject(packet, ipv4.ProtocolNumber)
}

// InjectPacket copies one complete IPv4 or IPv6 packet into gVisor.
func (s *Stack) InjectPacket(packet []byte) error {
	if len(packet) == 0 {
		return errors.New("netstack input is empty")
	}
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < header.IPv4MinimumSize {
			return errors.New("netstack input is not a complete IPv4 packet")
		}
		return s.inject(packet, ipv4.ProtocolNumber)
	case 6:
		if len(packet) < header.IPv6MinimumSize {
			return errors.New("netstack input is not a complete IPv6 packet")
		}
		return s.inject(packet, ipv6.ProtocolNumber)
	default:
		return errors.New("netstack input has an unsupported IP version")
	}
}

func (s *Stack) inject(packet []byte, protocol tcpip.NetworkProtocolNumber) error {
	select {
	case <-s.ctx.Done():
		return net.ErrClosed
	default:
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
	s.link.InjectInbound(protocol, pkt)
	pkt.DecRef()
	s.metrics.injectedPackets.Add(1)
	s.metrics.injectedBytes.Add(uint64(len(packet)))
	return nil
}

// ReadPacket waits for one outbound IPv4 packet. The returned slice is owned
// by the caller and remains valid after the next call.
func (s *Stack) ReadPacket(ctx context.Context) ([]byte, error) {
	readCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	pkt := s.link.ReadContext(readCtx)
	if pkt == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
	defer pkt.DecRef()
	view := pkt.ToView()
	packet := append([]byte(nil), view.AsSlice()...)
	view.Release()
	if len(packet) == 0 {
		return nil, errors.New("netstack emitted an empty packet")
	}
	s.metrics.emittedPackets.Add(1)
	s.metrics.emittedBytes.Add(uint64(len(packet)))
	return packet, nil
}

func (s *Stack) handleTCP(request *tcp.ForwarderRequest) {
	id := request.ID()
	flow, err := flowFromID(id)
	if err != nil {
		request.Complete(true)
		s.metrics.rejectedTCPFlows.Add(1)
		return
	}
	if !s.tryAcquireTCPSlot() {
		request.Complete(true)
		s.metrics.rejectedTCPFlows.Add(1)
		return
	}
	defer s.releaseTCPSlot()

	var queue waiter.Queue
	endpoint, tcpErr := request.CreateEndpoint(&queue)
	if tcpErr != nil {
		request.Complete(true)
		s.metrics.rejectedTCPFlows.Add(1)
		return
	}
	request.Complete(false)
	conn := gonet.NewTCPConn(&queue, endpoint)
	s.metrics.totalTCPFlows.Add(1)
	s.metrics.activeTCPFlows.Add(1)
	s.flows.Add(1)
	defer func() {
		_ = conn.Close()
		s.metrics.activeTCPFlows.Add(^uint64(0))
		s.flows.Done()
	}()
	if err := s.handler(s.ctx, flow, conn); err != nil && s.ctx.Err() == nil {
		s.metrics.tcpHandlerErrors.Add(1)
	}
}

func (s *Stack) tryAcquireTCPSlot() bool {
	select {
	case s.tcpSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Stack) releaseTCPSlot() {
	<-s.tcpSlots
}

func (s *Stack) handleUDP(request *udp.ForwarderRequest) {
	flow, err := udpFlowFromID(request.ID())
	if err != nil {
		s.metrics.rejectedUDPSessions.Add(1)
		return
	}
	var queue waiter.Queue
	endpoint, udpErr := request.CreateEndpoint(&queue)
	if udpErr != nil {
		s.metrics.rejectedUDPSessions.Add(1)
		return
	}
	conn := gonet.NewUDPConn(&queue, endpoint)
	s.metrics.totalUDPSessions.Add(1)
	s.metrics.activeUDPSessions.Add(1)
	s.flows.Add(1)
	go func() {
		defer func() {
			_ = conn.Close()
			s.metrics.activeUDPSessions.Add(^uint64(0))
			s.flows.Done()
		}()
		if err := s.udpHandler(s.ctx, flow, conn); err != nil && s.ctx.Err() == nil {
			s.metrics.udpHandlerErrors.Add(1)
		}
	}()
}

func flowFromID(id stack.TransportEndpointID) (TCPFlow, error) {
	local := id.LocalAddress
	remote := id.RemoteAddress
	localAddress, remoteAddress, err := addressesFromID(local, remote)
	if err != nil {
		return TCPFlow{}, err
	}
	return TCPFlow{
		SourceIP:        remoteAddress,
		SourcePort:      id.RemotePort,
		DestinationIP:   localAddress,
		DestinationPort: id.LocalPort,
	}, nil
}

func udpFlowFromID(id stack.TransportEndpointID) (UDPFlow, error) {
	local := id.LocalAddress
	remote := id.RemoteAddress
	localAddress, remoteAddress, err := addressesFromID(local, remote)
	if err != nil {
		return UDPFlow{}, err
	}
	return UDPFlow{
		SourceIP:        remoteAddress,
		SourcePort:      id.RemotePort,
		DestinationIP:   localAddress,
		DestinationPort: id.LocalPort,
	}, nil
}

func addressesFromID(local, remote tcpip.Address) (netip.Addr, netip.Addr, error) {
	if local.Len() != remote.Len() {
		return netip.Addr{}, netip.Addr{}, errors.New("flow address families differ")
	}
	switch local.Len() {
	case 4:
		return netip.AddrFrom4(local.As4()), netip.AddrFrom4(remote.As4()), nil
	case 16:
		return netip.AddrFrom16(local.As16()), netip.AddrFrom16(remote.As16()), nil
	default:
		return netip.Addr{}, netip.Addr{}, errors.New("flow has unsupported address family")
	}
}

func (s *Stack) Stats() Stats {
	return Stats{
		InjectedPackets:     s.metrics.injectedPackets.Load(),
		InjectedBytes:       s.metrics.injectedBytes.Load(),
		EmittedPackets:      s.metrics.emittedPackets.Load(),
		EmittedBytes:        s.metrics.emittedBytes.Load(),
		ActiveTCPFlows:      s.metrics.activeTCPFlows.Load(),
		TotalTCPFlows:       s.metrics.totalTCPFlows.Load(),
		RejectedTCPFlows:    s.metrics.rejectedTCPFlows.Load(),
		TCPHandlerErrors:    s.metrics.tcpHandlerErrors.Load(),
		ActiveUDPSessions:   s.metrics.activeUDPSessions.Load(),
		TotalUDPSessions:    s.metrics.totalUDPSessions.Load(),
		RejectedUDPSessions: s.metrics.rejectedUDPSessions.Load(),
		UDPHandlerErrors:    s.metrics.udpHandlerErrors.Load(),
	}
}

// Close cancels active handlers, closes endpoints, and waits for all handler
// goroutines to exit.
func (s *Stack) Close() error {
	s.closeOnce.Do(s.closeResources)
	s.flows.Wait()
	return nil
}

func (s *Stack) closeResources() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.link != nil {
		s.link.Close()
	}
	if s.stack != nil {
		s.stack.Close()
		s.stack.Wait()
	}
}
