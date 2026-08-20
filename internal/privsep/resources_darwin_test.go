//go:build darwin

package privsep

import (
	"io"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenControlFileProvidesPollableConnection(t *testing.T) {
	left, right := socketPairFiles(t)
	connection, err := openControlFile(left, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	peer, err := openControlFile(right, "test peer")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	want := []byte("private-control")
	go func() { _, _ = peer.Write(want) }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(connection, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("control payload = %q, want %q", got, want)
	}
}

func TestOpenDNSFilesReconstructsListeners(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udpFile, err := udp.File()
	if err != nil {
		t.Fatal(err)
	}
	tcpFile, err := tcp.File()
	if err != nil {
		udpFile.Close()
		t.Fatal(err)
	}

	listeners, err := openDNSFiles(udpFile, tcpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer listeners.UDP.Close()
	defer listeners.TCP.Close()
	if listeners.UDP.LocalAddr().String() != udp.LocalAddr().String() {
		t.Fatalf("UDP address = %s, want %s", listeners.UDP.LocalAddr(), udp.LocalAddr())
	}
	if listeners.TCP.Addr().String() != tcp.Addr().String() {
		t.Fatalf("TCP address = %s, want %s", listeners.TCP.Addr(), tcp.Addr())
	}
}

func TestOpenDNSFilesRequiresBothDescriptors(t *testing.T) {
	if _, err := openDNSFiles(nil, nil); err == nil {
		t.Fatal("missing DNS descriptors were accepted")
	}
}

func socketPairFiles(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(fds[0]), "test-control-left"), os.NewFile(uintptr(fds[1]), "test-control-right")
}
