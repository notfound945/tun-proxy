// Package interfaceinfo provides platform-neutral snapshots of network
// interfaces for diagnostics and configuration validation.
package interfaceinfo

import (
	"fmt"
	"net"
	"sort"
)

// Interface is an immutable snapshot of a network interface.
type Interface struct {
	Name         string
	Index        int
	MTU          int
	HardwareAddr string
	Addresses    []string
	Flags        net.Flags
}

// Up reports whether the interface has the UP flag.
func (i Interface) Up() bool { return i.Flags&net.FlagUp != 0 }

// Running reports whether the operating system considers the interface
// operational.
func (i Interface) Running() bool { return i.Flags&net.FlagRunning != 0 }

// List returns interface snapshots sorted by system index.
func List() ([]Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}

	result := make([]Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list addresses for interface %q: %w", iface.Name, err)
		}

		item := Interface{
			Name:         iface.Name,
			Index:        iface.Index,
			MTU:          iface.MTU,
			HardwareAddr: iface.HardwareAddr.String(),
			Flags:        iface.Flags,
			Addresses:    make([]string, 0, len(addresses)),
		}
		for _, address := range addresses {
			item.Addresses = append(item.Addresses, address.String())
		}
		sort.Strings(item.Addresses)
		result = append(result, item)
	}

	sort.Slice(result, func(a, b int) bool { return result[a].Index < result[b].Index })
	return result, nil
}

// Get returns one current interface snapshot by name.
func Get(name string) (Interface, error) {
	interfaces, err := List()
	if err != nil {
		return Interface{}, err
	}
	for _, iface := range interfaces {
		if iface.Name == name {
			return iface, nil
		}
	}
	return Interface{}, fmt.Errorf("network interface %q does not exist", name)
}
