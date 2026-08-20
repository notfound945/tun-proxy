package netstack

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

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
)

func TestTCPForwarderEchoOverMemoryLink(t *testing.T) {
	flowCh := make(chan TCPFlow, 1)
	proxy, err := New(Config{MTU: 1500, PacketQueue: 128, MaxTCPFlows: 16}, func(_ context.Context, flow TCPFlow, conn net.Conn) error {
		flowCh <- flow
		buffer := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				if _, writeErr := conn.Write(buffer[:n]); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}, func(context.Context, UDPFlow, net.Conn) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	clientStack, clientLink := newTestClientStack(t, [4]byte{10, 0, 0, 2})
	defer func() {
		clientLink.Close()
		clientStack.Close()
		clientStack.Wait()
	}()

	bridgeCtx, cancelBridge := context.WithCancel(context.Background())
	defer cancelBridge()
	bridgeErr := make(chan error, 2)
	go bridgeClientToProxy(bridgeCtx, clientLink, proxy, bridgeErr)
	go bridgeProxyToClient(bridgeCtx, proxy, clientLink, bridgeErr)

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDial()
	target := [4]byte{198, 18, 0, 10}
	conn, err := gonet.DialContextTCP(dialCtx, clientStack, tcpip.FullAddress{
		Addr: tcpip.AddrFrom4(target),
		Port: 8443,
	}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial in-memory forwarded TCP: %v", err)
	}
	defer conn.Close()

	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatalf("read echoed payload: %v", err)
	}
	if string(received) != string(payload) {
		t.Fatal("echoed payload differs")
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("half-close client: %v", err)
	}

	select {
	case flow := <-flowCh:
		if flow.SourceIP != netip.MustParseAddr("10.0.0.2") || flow.DestinationIP != netip.MustParseAddr("198.18.0.10") {
			t.Fatalf("unexpected flow addresses: %+v", flow)
		}
		if flow.DestinationPort != 8443 || flow.SourcePort == 0 {
			t.Fatalf("unexpected flow ports: %+v", flow)
		}
	case <-time.After(time.Second):
		t.Fatal("TCP handler did not receive flow metadata")
	}

	stats := proxy.Stats()
	if stats.TotalTCPFlows != 1 || stats.InjectedPackets == 0 || stats.EmittedPackets == 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	cancelBridge()
	for range 2 {
		if err := <-bridgeErr; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("packet bridge: %v", err)
		}
	}
}

func TestTCPFlowSlotsEnforceConfiguredLimit(t *testing.T) {
	proxy := &Stack{tcpSlots: make(chan struct{}, 1)}
	if !proxy.tryAcquireTCPSlot() {
		t.Fatal("first TCP flow should acquire the only slot")
	}
	if proxy.tryAcquireTCPSlot() {
		t.Fatal("second TCP flow acquired a slot past the configured limit")
	}

	proxy.releaseTCPSlot()
	if !proxy.tryAcquireTCPSlot() {
		t.Fatal("TCP flow slot was not reusable after release")
	}
	proxy.releaseTCPSlot()
}

func TestUDPForwarderEchoOverMemoryLink(t *testing.T) {
	flowCh := make(chan UDPFlow, 1)
	proxy, err := New(
		Config{MTU: 1500, PacketQueue: 128, MaxTCPFlows: 16},
		func(context.Context, TCPFlow, net.Conn) error { return nil },
		func(_ context.Context, flow UDPFlow, conn net.Conn) error {
			flowCh <- flow
			buffer := make([]byte, 2048)
			n, err := conn.Read(buffer)
			if err != nil {
				return err
			}
			_, err = conn.Write(buffer[:n])
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	clientStack, clientLink := newTestClientStack(t, [4]byte{10, 0, 0, 2})
	defer func() {
		clientLink.Close()
		clientStack.Close()
		clientStack.Wait()
	}()
	bridgeCtx, cancelBridge := context.WithCancel(context.Background())
	bridgeErr := make(chan error, 2)
	go bridgeClientToProxy(bridgeCtx, clientLink, proxy, bridgeErr)
	go bridgeProxyToClient(bridgeCtx, proxy, clientLink, bridgeErr)

	target := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 20}), Port: 5353}
	conn, err := gonet.DialUDP(clientStack, nil, &target, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	payload := []byte("udp preserves one datagram")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, 2048)
	n, err := conn.Read(received)
	if err != nil {
		t.Fatal(err)
	}
	if string(received[:n]) != string(payload) {
		t.Fatalf("echo = %q, want %q", received[:n], payload)
	}
	select {
	case flow := <-flowCh:
		if flow.SourceIP != netip.MustParseAddr("10.0.0.2") || flow.DestinationIP != netip.MustParseAddr("198.18.0.20") || flow.DestinationPort != 5353 {
			t.Fatalf("unexpected UDP flow: %+v", flow)
		}
	case <-time.After(time.Second):
		t.Fatal("UDP handler did not receive flow metadata")
	}
	stats := proxy.Stats()
	if stats.TotalUDPSessions != 1 || stats.InjectedPackets == 0 || stats.EmittedPackets == 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	cancelBridge()
	for range 2 {
		if err := <-bridgeErr; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("packet bridge: %v", err)
		}
	}
}

func TestIPv6TCPAndUDPForwardersOverMemoryLink(t *testing.T) {
	tcpFlows := make(chan TCPFlow, 1)
	udpFlows := make(chan UDPFlow, 1)
	proxy, err := New(
		Config{MTU: 1500, PacketQueue: 128, MaxTCPFlows: 16},
		func(_ context.Context, flow TCPFlow, conn net.Conn) error {
			tcpFlows <- flow
			buffer := make([]byte, 64)
			count, err := conn.Read(buffer)
			if err != nil {
				return err
			}
			_, err = conn.Write(buffer[:count])
			return err
		},
		func(_ context.Context, flow UDPFlow, conn net.Conn) error {
			udpFlows <- flow
			buffer := make([]byte, 64)
			count, err := conn.Read(buffer)
			if err != nil {
				return err
			}
			_, err = conn.Write(buffer[:count])
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	clientAddress := netip.MustParseAddr("fd00:7::2")
	clientStack, clientLink := newTestIPv6ClientStack(t, clientAddress)
	defer func() {
		clientLink.Close()
		clientStack.Close()
		clientStack.Wait()
	}()
	bridgeCtx, cancelBridge := context.WithCancel(context.Background())
	bridgeErr := make(chan error, 2)
	go bridgeClientToProxy(bridgeCtx, clientLink, proxy, bridgeErr)
	go bridgeProxyToClient(bridgeCtx, proxy, clientLink, bridgeErr)

	targetAddress := netip.MustParseAddr("fd00:8::a")
	targetBytes := targetAddress.As16()
	tcpConnection, err := gonet.DialContextTCP(t.Context(), clientStack, tcpip.FullAddress{
		Addr: tcpip.AddrFrom16(targetBytes), Port: 8443,
	}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	tcpPayload := []byte("tcp6")
	if _, err := tcpConnection.Write(tcpPayload); err != nil {
		t.Fatal(err)
	}
	tcpReply := make([]byte, len(tcpPayload))
	if _, err := io.ReadFull(tcpConnection, tcpReply); err != nil {
		t.Fatal(err)
	}
	_ = tcpConnection.Close()
	if flow := <-tcpFlows; flow.SourceIP != clientAddress || flow.DestinationIP != targetAddress || flow.DestinationPort != 8443 {
		t.Fatalf("TCP flow = %+v", flow)
	}

	udpTarget := tcpip.FullAddress{Addr: tcpip.AddrFrom16(targetBytes), Port: 5353}
	udpConnection, err := gonet.DialUDP(clientStack, nil, &udpTarget, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer udpConnection.Close()
	_ = udpConnection.SetDeadline(time.Now().Add(3 * time.Second))
	udpPayload := []byte("udp6")
	if _, err := udpConnection.Write(udpPayload); err != nil {
		t.Fatal(err)
	}
	udpReply := make([]byte, len(udpPayload))
	if _, err := io.ReadFull(udpConnection, udpReply); err != nil {
		t.Fatal(err)
	}
	if flow := <-udpFlows; flow.SourceIP != clientAddress || flow.DestinationIP != targetAddress || flow.DestinationPort != 5353 {
		t.Fatalf("UDP flow = %+v", flow)
	}

	cancelBridge()
	for range 2 {
		if err := <-bridgeErr; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("packet bridge: %v", err)
		}
	}
}

func newTestClientStack(t *testing.T, address [4]byte) (*stack.Stack, *channel.Endpoint) {
	t.Helper()
	clientStack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(128, 1500, "")
	if err := clientStack.CreateNIC(nicID, link); err != nil {
		t.Fatalf("create client NIC: %s", err)
	}
	if err := clientStack.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFrom4(address),
			PrefixLen: 24,
		},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("add client address: %s", err)
	}
	clientStack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	return clientStack, link
}

func newTestIPv6ClientStack(t *testing.T, address netip.Addr) (*stack.Stack, *channel.Endpoint) {
	t.Helper()
	clientStack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(128, 1500, "")
	if err := clientStack.CreateNIC(nicID, link); err != nil {
		t.Fatalf("create IPv6 client NIC: %s", err)
	}
	addressBytes := address.As16()
	if err := clientStack.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address: tcpip.AddrFrom16(addressBytes), PrefixLen: 64,
		},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("add IPv6 client address: %s", err)
	}
	clientStack.SetRouteTable([]tcpip.Route{{Destination: header.IPv6EmptySubnet, NIC: nicID}})
	return clientStack, link
}

func bridgeClientToProxy(ctx context.Context, link *channel.Endpoint, proxy *Stack, result chan<- error) {
	for {
		packet := link.ReadContext(ctx)
		if packet == nil {
			result <- ctx.Err()
			return
		}
		view := packet.ToView()
		err := proxy.InjectPacket(view.AsSlice())
		view.Release()
		packet.DecRef()
		if err != nil {
			result <- err
			return
		}
	}
}

func bridgeProxyToClient(ctx context.Context, proxy *Stack, link *channel.Endpoint, result chan<- error) {
	for {
		bytes, err := proxy.ReadPacket(ctx)
		if err != nil {
			result <- err
			return
		}
		packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(bytes)})
		protocol := ipv4.ProtocolNumber
		if len(bytes) != 0 && bytes[0]>>4 == 6 {
			protocol = ipv6.ProtocolNumber
		}
		link.InjectInbound(protocol, packet)
		packet.DecRef()
	}
}
