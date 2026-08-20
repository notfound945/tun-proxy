package tun

import (
	"errors"
	"os"
	"testing"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

func TestReadPacketUsesDarwinPacketOffset(t *testing.T) {
	device := &recordingDevice{payload: []byte{0x45, 0, 0, 20}}
	buffer := make([]byte, 64)

	packet, err := ReadPacket(device, buffer)
	if err != nil {
		t.Fatal(err)
	}
	if device.offset != PacketOffset {
		t.Fatalf("Read offset = %d, want %d", device.offset, PacketOffset)
	}
	if got, want := string(packet), string(device.payload); got != want {
		t.Fatalf("packet = %v, want %v", packet, device.payload)
	}
}

func TestReadPacketRejectsShortBuffer(t *testing.T) {
	_, err := ReadPacket(&recordingDevice{}, make([]byte, PacketOffset))
	if err == nil {
		t.Fatal("ReadPacket returned nil for a buffer without payload space")
	}
}

type recordingDevice struct {
	offset  int
	payload []byte
}

func (d *recordingDevice) File() *os.File                       { return nil }
func (d *recordingDevice) Write(_ [][]byte, _ int) (int, error) { return 0, errors.ErrUnsupported }
func (d *recordingDevice) MTU() (int, error)                    { return 0, nil }
func (d *recordingDevice) Name() (string, error)                { return "test", nil }
func (d *recordingDevice) Events() <-chan wgtun.Event           { return nil }
func (d *recordingDevice) Close() error                         { return nil }
func (d *recordingDevice) BatchSize() int                       { return 1 }
func (d *recordingDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	d.offset = offset
	copy(bufs[0][offset:], d.payload)
	sizes[0] = len(d.payload)
	return 1, nil
}
