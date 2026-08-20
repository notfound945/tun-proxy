// Package tun isolates the packet-buffer conventions of the selected utun
// implementation from the rest of the application.
package tun

import (
	"fmt"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

// PacketOffset reserves the four-byte address-family header used by Darwin
// utun. wireguard/tun consumes this prefix internally but requires the caller
// to provide it for both Read and Write.
const PacketOffset = 4

// ReadPacket reads one IP packet and returns a payload view without Darwin's
// packet-info prefix. buffer must include PacketOffset bytes before the
// maximum payload size.
func ReadPacket(device wgtun.Device, buffer []byte) ([]byte, error) {
	if len(buffer) <= PacketOffset {
		return nil, fmt.Errorf("TUN read buffer has length %d, want more than %d", len(buffer), PacketOffset)
	}

	buffers := [][]byte{buffer}
	sizes := make([]int, 1)
	count, err := device.Read(buffers, sizes, PacketOffset)
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("TUN read returned %d packets, want 1", count)
	}
	if sizes[0] < 0 || sizes[0] > len(buffer)-PacketOffset {
		return nil, fmt.Errorf("TUN read returned invalid packet size %d", sizes[0])
	}
	return buffer[PacketOffset : PacketOffset+sizes[0]], nil
}

// WritePacket writes one payload already stored after PacketOffset bytes in
// buffer. The selected Darwin implementation fills the reserved prefix with
// the correct address family before writing to utun.
func WritePacket(device wgtun.Device, buffer []byte, payloadLength int) error {
	if payloadLength <= 0 || payloadLength > len(buffer)-PacketOffset {
		return fmt.Errorf("invalid TUN write payload length %d for buffer length %d", payloadLength, len(buffer))
	}
	written, err := device.Write([][]byte{buffer[:PacketOffset+payloadLength]}, PacketOffset)
	if err != nil {
		return err
	}
	if written != 1 {
		return fmt.Errorf("TUN write accepted %d packets, want 1", written)
	}
	return nil
}
