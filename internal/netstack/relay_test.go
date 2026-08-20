package netstack

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestRelayPreservesTCPHalfClose(t *testing.T) {
	app, relayClient := tcpPair(t)
	relayUpstream, server := tcpPair(t)
	defer app.Close()
	defer server.Close()

	relayDone := make(chan error, 1)
	go func() {
		relayDone <- Relay(context.Background(), relayClient, relayUpstream, time.Second)
	}()

	request := []byte("request body")
	if _, err := app.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	gotRequest, err := io.ReadAll(server)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRequest) != string(request) {
		t.Fatalf("request = %q, want %q", gotRequest, request)
	}

	response := []byte("response after request EOF")
	if _, err := server.Write(response); err != nil {
		t.Fatal(err)
	}
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	gotResponse, err := io.ReadAll(app)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotResponse) != string(response) {
		t.Fatalf("response = %q, want %q", gotResponse, response)
	}
	if err := <-relayDone; err != nil {
		t.Fatalf("relay: %v", err)
	}
}

func TestRelayCancellationStopsBothDirections(t *testing.T) {
	leftRelay, leftPeer := net.Pipe()
	rightRelay, rightPeer := net.Pipe()
	defer leftPeer.Close()
	defer rightPeer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Relay(ctx, leftRelay, rightRelay, time.Second) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Relay error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Relay leaked copy goroutines after cancellation")
	}
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server := <-accepted:
		return client, server
	case err := <-acceptErr:
		client.Close()
		t.Fatal(err)
		return nil, nil
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("accept timed out")
		return nil, nil
	}
}
