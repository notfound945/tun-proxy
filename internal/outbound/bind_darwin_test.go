//go:build darwin

package outbound

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"
)

func TestInterfaceControlRejectsUnknownInterface(t *testing.T) {
	control := InterfaceControl("tun-proxy-interface-that-does-not-exist")
	var raw syscall.RawConn
	if err := control("tcp4", "127.0.0.1:1", raw); err == nil {
		t.Fatal("InterfaceControl returned nil for an unknown interface")
	}
}

func TestDirectUDPDialRejectsUnknownInterfaceAsRecoverable(t *testing.T) {
	direct, err := NewDirect("missing", "tun-proxy-interface-that-does-not-exist", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := direct.DialPacket(context.Background(), netip.MustParseAddrPort("192.0.2.1:443"))
	if conn != nil {
		conn.Close()
		t.Fatal("DialPacket unexpectedly returned a connection")
	}
	if err == nil || !IsRecoverable(err) {
		t.Fatalf("DialPacket error = %v, want recoverable interface error", err)
	}
}

func TestDirectIPv6DialRejectsUnknownInterfaceAsRecoverable(t *testing.T) {
	direct, err := NewDirect("missing", "tun-proxy-interface-that-does-not-exist", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := direct.DialContext(context.Background(), "tcp6", netip.MustParseAddrPort("[2001:db8::1]:443"))
	if conn != nil {
		conn.Close()
		t.Fatal("DialContext unexpectedly returned a connection")
	}
	if err == nil || !IsRecoverable(err) {
		t.Fatalf("DialContext error = %v, want recoverable interface error", err)
	}
}

func TestDirectRejectsNetworkDestinationFamilyMismatch(t *testing.T) {
	direct, err := NewDirect("test", "lo0", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := direct.DialContext(context.Background(), "tcp4", netip.MustParseAddrPort("[::1]:443"))
	if conn != nil {
		conn.Close()
		t.Fatal("DialContext unexpectedly returned a connection")
	}
	if err == nil {
		t.Fatal("DialContext accepted a mismatched network and destination family")
	}
}

func TestDirectIPv6TCPAndUDPOnLoopback(t *testing.T) {
	direct, err := NewDirect("loopback", "lo0", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	tcpDestination, err := netip.ParseAddrPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tcpConnection, err := direct.DialContext(t.Context(), "tcp6", tcpDestination)
	if err != nil {
		t.Fatal(err)
	}
	_ = tcpConnection.Close()

	packetConnection, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConnection.Close()
	udpDestination, err := netip.ParseAddrPort(packetConnection.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	udpConnection, err := direct.DialPacket(t.Context(), udpDestination)
	if err != nil {
		t.Fatal(err)
	}
	defer udpConnection.Close()
	if _, err := udpConnection.Write([]byte("phase8")); err != nil {
		t.Fatal(err)
	}
	if err := packetConnection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16)
	count, _, err := packetConnection.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(buffer[:count]) != "phase8" {
		t.Fatalf("UDP payload = %q", buffer[:count])
	}
}

func TestInterfaceControlRejectsAmbiguousNetwork(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	raw, err := listener.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	control := InterfaceControl("lo0")
	if err := control("tcp", listener.Addr().String(), raw); err == nil {
		t.Fatal("InterfaceControl returned nil for an ambiguous network")
	}
}
