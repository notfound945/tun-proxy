package tun

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
)

type PacketHandler func(ctx context.Context, packet []byte) error

type Stats struct {
	ReceivedPackets    uint64
	ReceivedBytes      uint64
	TransmittedPackets uint64
	TransmittedBytes   uint64
	DroppedMalformed   uint64
	DroppedIPv6        uint64
	ReadErrors         uint64
	WriteErrors        uint64
	HandlerErrors      uint64
}

type counters struct {
	receivedPackets    atomic.Uint64
	receivedBytes      atomic.Uint64
	transmittedPackets atomic.Uint64
	transmittedBytes   atomic.Uint64
	droppedMalformed   atomic.Uint64
	droppedIPv6        atomic.Uint64
	readErrors         atomic.Uint64
	writeErrors        atomic.Uint64
	handlerErrors      atomic.Uint64
}

// Pump owns device while Run is active. Canceling the context closes the
// device to wake a blocked read. PacketHandler is synchronous and must not
// retain packet after returning because its bounded buffer is immediately
// reused.
type Pump struct {
	device  *Device
	pool    *BufferPool
	handler PacketHandler
	metrics counters
}

func NewPump(device *Device, pool *BufferPool, handler PacketHandler) (*Pump, error) {
	if device == nil || device.native == nil {
		return nil, errors.New("TUN device is required")
	}
	if pool == nil {
		return nil, errors.New("packet buffer pool is required")
	}
	if handler == nil {
		return nil, errors.New("packet handler is required")
	}
	return &Pump{device: device, pool: pool, handler: handler}, nil
}

func (pump *Pump) Run(ctx context.Context) error {
	stopClose := context.AfterFunc(ctx, func() { _ = pump.device.Close() })
	defer stopClose()

	for {
		buffer, err := pump.pool.Get(ctx)
		if err != nil {
			if ctx.Err() != nil || pump.device.Closed() {
				return nil
			}
			return err
		}
		packet, err := ReadPacket(pump.device.native, buffer)
		if err != nil {
			pump.pool.Put(buffer)
			if ctx.Err() != nil || pump.device.Closed() {
				return nil
			}
			pump.metrics.readErrors.Add(1)
			return fmt.Errorf("read utun packet: %w", err)
		}
		packet, valid := pump.validateInbound(packet)
		if !valid {
			pump.pool.Put(buffer)
			continue
		}
		pump.metrics.receivedPackets.Add(1)
		pump.metrics.receivedBytes.Add(uint64(len(packet)))
		if err := pump.handler(ctx, packet); err != nil {
			pump.metrics.handlerErrors.Add(1)
			pump.pool.Put(buffer)
			return fmt.Errorf("handle utun packet: %w", err)
		}
		pump.pool.Put(buffer)
	}
}

// Write copies an IPv4 or IPv6 packet into one bounded buffer and writes it to utun.
func (pump *Pump) Write(ctx context.Context, packet []byte) error {
	validated, valid := pump.validateIP(packet)
	if !valid {
		pump.metrics.droppedMalformed.Add(1)
		return errors.New("refuse malformed outbound IP packet")
	}
	packet = validated
	buffer, err := pump.pool.Get(ctx)
	if err != nil {
		return err
	}
	defer pump.pool.Put(buffer)
	if len(packet) > len(buffer)-PacketOffset {
		pump.metrics.writeErrors.Add(1)
		return fmt.Errorf("outbound packet length %d exceeds buffer payload capacity %d", len(packet), len(buffer)-PacketOffset)
	}
	copy(buffer[PacketOffset:], packet)
	if err := WritePacket(pump.device.native, buffer, len(packet)); err != nil {
		pump.metrics.writeErrors.Add(1)
		return fmt.Errorf("write utun packet: %w", err)
	}
	pump.metrics.transmittedPackets.Add(1)
	pump.metrics.transmittedBytes.Add(uint64(len(packet)))
	return nil
}

func (pump *Pump) validateInbound(packet []byte) ([]byte, bool) {
	if len(packet) == 0 {
		pump.metrics.droppedMalformed.Add(1)
		return nil, false
	}
	switch packet[0] >> 4 {
	case 4:
		validated, ok := pump.validateIPv4(packet)
		if !ok {
			pump.metrics.droppedMalformed.Add(1)
			return nil, false
		}
		return validated, true
	case 6:
		validated, ok := pump.validateIPv6(packet)
		if !ok {
			pump.metrics.droppedMalformed.Add(1)
			return nil, false
		}
		return validated, true
	default:
		pump.metrics.droppedMalformed.Add(1)
		return nil, false
	}
}

func (pump *Pump) validateIP(packet []byte) ([]byte, bool) {
	if len(packet) == 0 {
		return nil, false
	}
	if packet[0]>>4 == 6 {
		return pump.validateIPv6(packet)
	}
	return pump.validateIPv4(packet)
}

func (pump *Pump) validateIPv4(packet []byte) ([]byte, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return nil, false
	}
	headerLength := int(packet[0]&0x0f) * 4
	if headerLength < 20 || headerLength > len(packet) {
		return nil, false
	}
	totalLength := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLength < headerLength || totalLength > len(packet) {
		return nil, false
	}
	return packet[:totalLength], true
}

func (pump *Pump) validateIPv6(packet []byte) ([]byte, bool) {
	if len(packet) < 40 || packet[0]>>4 != 6 {
		return nil, false
	}
	totalLength := 40 + int(binary.BigEndian.Uint16(packet[4:6]))
	if totalLength > len(packet) {
		return nil, false
	}
	return packet[:totalLength], true
}

func (pump *Pump) Stats() Stats {
	return Stats{
		ReceivedPackets:    pump.metrics.receivedPackets.Load(),
		ReceivedBytes:      pump.metrics.receivedBytes.Load(),
		TransmittedPackets: pump.metrics.transmittedPackets.Load(),
		TransmittedBytes:   pump.metrics.transmittedBytes.Load(),
		DroppedMalformed:   pump.metrics.droppedMalformed.Load(),
		DroppedIPv6:        pump.metrics.droppedIPv6.Load(),
		ReadErrors:         pump.metrics.readErrors.Load(),
		WriteErrors:        pump.metrics.writeErrors.Load(),
		HandlerErrors:      pump.metrics.handlerErrors.Load(),
	}
}
