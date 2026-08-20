//go:build darwin

package privsep

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/hailinpan/tun-proxy/internal/fakedns"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
)

const (
	ControlFD = 3
	TUNFD     = 4
	UDPDNSFD  = 5
	TCPDNSFD  = 6
)

// WorkerResources are the only privileged resources inherited by the data
// plane worker. Opening them does not create or reconfigure host resources.
type WorkerResources struct {
	Control net.Conn
	Device  *internaltun.Device
	DNS     fakedns.Listeners
}

// OpenInheritedResources reconstructs the fixed service descriptors installed
// by the root supervisor. It takes ownership of every inherited descriptor,
// including when reconstruction fails.
func OpenInheritedResources() (*WorkerResources, error) {
	control, err := openControlFile(os.NewFile(ControlFD, "tun-proxy-control"), "inherited worker")
	if err != nil {
		return nil, err
	}
	device, err := internaltun.OpenFile(os.NewFile(TUNFD, "tun-proxy-utun"))
	if err != nil {
		_ = control.Close()
		return nil, err
	}
	listeners, err := openDNSFiles(
		os.NewFile(UDPDNSFD, "tun-proxy-dns-udp"),
		os.NewFile(TCPDNSFD, "tun-proxy-dns-tcp"),
	)
	if err != nil {
		_ = device.Close()
		_ = control.Close()
		return nil, err
	}
	return &WorkerResources{Control: control, Device: device, DNS: listeners}, nil
}

func (resources *WorkerResources) Close() error {
	if resources == nil {
		return nil
	}
	var failures []error
	if resources.DNS.UDP != nil {
		failures = append(failures, resources.DNS.UDP.Close())
		resources.DNS.UDP = nil
	}
	if resources.DNS.TCP != nil {
		failures = append(failures, resources.DNS.TCP.Close())
		resources.DNS.TCP = nil
	}
	if resources.Device != nil {
		failures = append(failures, resources.Device.Close())
		resources.Device = nil
	}
	if resources.Control != nil {
		failures = append(failures, resources.Control.Close())
		resources.Control = nil
	}
	return errors.Join(failures...)
}

func openControlFile(file *os.File, description string) (net.Conn, error) {
	if file == nil {
		return nil, fmt.Errorf("open %s control connection: nil file", description)
	}
	connection, err := net.FileConn(file)
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("open %s control connection: %w", description, err)
	}
	if closeErr != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("close original %s control descriptor: %w", description, closeErr)
	}
	return connection, nil
}

func openDNSFiles(udpFile, tcpFile *os.File) (fakedns.Listeners, error) {
	if udpFile == nil {
		if tcpFile != nil {
			_ = tcpFile.Close()
		}
		return fakedns.Listeners{}, errors.New("missing inherited UDP DNS descriptor")
	}
	udpConnection, err := net.FilePacketConn(udpFile)
	udpCloseErr := udpFile.Close()
	if err != nil {
		if tcpFile != nil {
			_ = tcpFile.Close()
		}
		return fakedns.Listeners{}, fmt.Errorf("open inherited UDP DNS descriptor: %w", err)
	}
	if udpCloseErr != nil {
		_ = udpConnection.Close()
		if tcpFile != nil {
			_ = tcpFile.Close()
		}
		return fakedns.Listeners{}, fmt.Errorf("close original UDP DNS descriptor: %w", udpCloseErr)
	}
	if tcpFile == nil {
		_ = udpConnection.Close()
		return fakedns.Listeners{}, errors.New("missing inherited TCP DNS descriptor")
	}
	tcpListener, err := net.FileListener(tcpFile)
	tcpCloseErr := tcpFile.Close()
	if err != nil {
		_ = udpConnection.Close()
		return fakedns.Listeners{}, fmt.Errorf("open inherited TCP DNS descriptor: %w", err)
	}
	if tcpCloseErr != nil {
		_ = tcpListener.Close()
		_ = udpConnection.Close()
		return fakedns.Listeners{}, fmt.Errorf("close original TCP DNS descriptor: %w", tcpCloseErr)
	}
	return fakedns.Listeners{UDP: udpConnection, TCP: tcpListener}, nil
}
