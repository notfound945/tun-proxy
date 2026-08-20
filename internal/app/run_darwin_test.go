//go:build darwin

package app

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type testPacketReader struct {
	mutex   sync.Mutex
	packets [][]byte
}

func (reader *testPacketReader) ReadPacket(ctx context.Context) ([]byte, error) {
	reader.mutex.Lock()
	if len(reader.packets) != 0 {
		packet := reader.packets[0]
		reader.packets = reader.packets[1:]
		reader.mutex.Unlock()
		return packet, nil
	}
	reader.mutex.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type testPacketWriter struct {
	written chan []byte
}

func (writer *testPacketWriter) Write(_ context.Context, packet []byte) error {
	writer.written <- append([]byte(nil), packet...)
	return nil
}

func TestPumpNetstackOutputWritesAndStopsOnCancellation(t *testing.T) {
	packet := make([]byte, 20)
	packet[0] = 0x45
	reader := &testPacketReader{packets: [][]byte{packet}}
	writer := &testPacketWriter{written: make(chan []byte, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pumpNetstackOutput(ctx, reader, writer) }()
	if got := <-writer.written; len(got) != len(packet) || got[0] != 0x45 {
		t.Fatalf("written packet = %v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pumpNetstackOutput = %v", err)
	}
}

func TestPumpNetstackOutputWritesIPv6(t *testing.T) {
	packet := make([]byte, 40)
	packet[0] = 0x60
	reader := &testPacketReader{packets: [][]byte{packet}}
	writer := &testPacketWriter{written: make(chan []byte, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pumpNetstackOutput(ctx, reader, writer) }()
	if got := <-writer.written; len(got) != len(packet) || got[0] != 0x60 {
		t.Fatalf("written packet = %v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pumpNetstackOutput = %v", err)
	}
}

func TestPumpNetstackOutputRejectsMalformedPacket(t *testing.T) {
	reader := &testPacketReader{packets: [][]byte{{0x60, 0, 0}}}
	err := pumpNetstackOutput(t.Context(), reader, &testPacketWriter{written: make(chan []byte, 1)})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("pumpNetstackOutput error = %v", err)
	}
}
