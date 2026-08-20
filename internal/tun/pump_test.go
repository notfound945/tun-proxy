package tun

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

func TestPumpHandlesIPv4AndIPv6(t *testing.T) {
	native := newFakeNativeDevice("utun-test")
	native.reads <- ipv6Packet(0)
	native.reads <- ipv4Packet(20)
	device := &Device{native: native, name: "utun-test"}
	pool, err := NewBufferPool(256, 4)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	handled := 0
	pump, err := NewPump(device, pool, func(_ context.Context, packet []byte) error {
		handled++
		if len(packet) != 40 && len(packet) != 20 {
			t.Fatalf("packet length = %d, want 20 or 40", len(packet))
		}
		if handled == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pump.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stats := pump.Stats()
	if handled != 2 || stats.ReceivedPackets != 2 || stats.DroppedIPv6 != 0 {
		t.Fatalf("handled=%d stats=%+v", handled, stats)
	}
	if pool.Available() != pool.Capacity() {
		t.Fatalf("pool available = %d, want %d", pool.Available(), pool.Capacity())
	}
}

func TestPumpWriteUsesReservedOffset(t *testing.T) {
	native := newFakeNativeDevice("utun-test")
	device := &Device{native: native, name: "utun-test"}
	pool, err := NewBufferPool(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	pump, err := NewPump(device, pool, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	packet := ipv4Packet(20)
	if err := pump.Write(t.Context(), packet); err != nil {
		t.Fatal(err)
	}
	if native.writeOffset != PacketOffset {
		t.Fatalf("write offset = %d, want %d", native.writeOffset, PacketOffset)
	}
	if got := <-native.writes; string(got) != string(packet) {
		t.Fatalf("written packet = %v, want %v", got, packet)
	}
	if stats := pump.Stats(); stats.TransmittedPackets != 1 || stats.TransmittedBytes != 20 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestPumpWritesIPv6UsingReservedOffset(t *testing.T) {
	native := newFakeNativeDevice("utun-test")
	device := &Device{native: native, name: "utun-test"}
	pool, err := NewBufferPool(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	pump, err := NewPump(device, pool, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	packet := ipv6Packet(16)
	if err := pump.Write(t.Context(), packet); err != nil {
		t.Fatal(err)
	}
	if native.writeOffset != PacketOffset {
		t.Fatalf("write offset = %d, want %d", native.writeOffset, PacketOffset)
	}
	if got := <-native.writes; string(got) != string(packet) {
		t.Fatalf("written packet = %v, want %v", got, packet)
	}
}

func TestPumpRejectsMalformedPacket(t *testing.T) {
	native := newFakeNativeDevice("utun-test")
	pool, err := NewBufferPool(64, 2)
	if err != nil {
		t.Fatal(err)
	}
	pump, err := NewPump(&Device{native: native}, pool, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := pump.Write(t.Context(), []byte{0x45}); err == nil {
		t.Fatal("Write accepted a malformed packet")
	}
	if pump.Stats().DroppedMalformed != 1 {
		t.Fatalf("stats = %+v", pump.Stats())
	}
}

func TestBufferPoolIsBounded(t *testing.T) {
	pool, err := NewBufferPool(64, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := pool.Get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("third Get = %v, want context cancellation", err)
	}
	pool.Put(first)
	pool.Put(second)
	pool.Put(make([]byte, 64))
	if pool.Available() != 2 {
		t.Fatalf("pool grew to %d", pool.Available())
	}
}

func ipv4Packet(length int) []byte {
	packet := make([]byte, length)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(length))
	packet[8] = 64
	packet[9] = 6
	return packet
}

func ipv6Packet(payloadLength int) []byte {
	packet := make([]byte, 40+payloadLength)
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], uint16(payloadLength))
	packet[6] = 6
	packet[7] = 64
	return packet
}

type fakeNativeDevice struct {
	name        string
	reads       chan []byte
	writes      chan []byte
	writeOffset int
	closed      chan struct{}
	closeOnce   sync.Once
}

func newFakeNativeDevice(name string) *fakeNativeDevice {
	return &fakeNativeDevice{
		name:   name,
		reads:  make(chan []byte, 8),
		writes: make(chan []byte, 8),
		closed: make(chan struct{}),
	}
}

func (device *fakeNativeDevice) File() *os.File { return nil }

func (device *fakeNativeDevice) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	select {
	case packet := <-device.reads:
		copy(buffers[0][offset:], packet)
		sizes[0] = len(packet)
		return 1, nil
	case <-device.closed:
		return 0, os.ErrClosed
	}
}

func (device *fakeNativeDevice) Write(buffers [][]byte, offset int) (int, error) {
	device.writeOffset = offset
	device.writes <- append([]byte(nil), buffers[0][offset:]...)
	return 1, nil
}

func (device *fakeNativeDevice) MTU() (int, error)          { return 1400, nil }
func (device *fakeNativeDevice) Name() (string, error)      { return device.name, nil }
func (device *fakeNativeDevice) Events() <-chan wgtun.Event { return nil }
func (device *fakeNativeDevice) BatchSize() int             { return 1 }
func (device *fakeNativeDevice) Close() error {
	device.closeOnce.Do(func() { close(device.closed) })
	return nil
}
