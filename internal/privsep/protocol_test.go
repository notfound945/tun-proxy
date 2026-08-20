package privsep

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

const testDigest = "sha256:09bfcc6a14b83e2192b8673677725c84883ee9cd0c70e45c9ec09daa8f2b2847"

func validBootstrap() Bootstrap {
	return Bootstrap{
		Config: []byte("version: 1\n"), ConfigDigest: testDigest, TUNName: "utun7",
		DNSListen: "127.0.0.1:53", StatusSocket: "/var/run/tun-proxy/worker/status.sock",
		InterfaceDNS: map[string][]netip.AddrPort{"en0": {netip.MustParseAddrPort("192.168.1.1:53")}},
	}
}

func TestCodecRoundTripAndStrictPayload(t *testing.T) {
	var wire bytes.Buffer
	codec, err := NewCodec(&wire, &wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Send(KindPrepared, 7, Prepared{PID: 42, UID: 501, GID: 20}); err != nil {
		t.Fatal(err)
	}
	message, err := codec.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind != KindPrepared || message.RequestID != 7 || message.Version != ProtocolVersion {
		t.Fatalf("message = %+v", message)
	}
	payload, err := DecodePayload[Prepared](message)
	if err != nil {
		t.Fatal(err)
	}
	if payload.PID != 42 || payload.UID != 501 || payload.GID != 20 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestCodecRejectsOversizedAndUnknownFrames(t *testing.T) {
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, maxFrameSize+1)
	codec, _ := NewCodec(bytes.NewReader(header), &bytes.Buffer{})
	if _, err := codec.Receive(); err == nil || !strings.Contains(err.Error(), "frame size") {
		t.Fatalf("oversized Receive() error = %v", err)
	}

	var wire bytes.Buffer
	contents := []byte(`{"version":1,"kind":"surprise","payload":{}}`)
	binary.BigEndian.PutUint32(header, uint32(len(contents)))
	wire.Write(header)
	wire.Write(contents)
	codec, _ = NewCodec(&wire, &bytes.Buffer{})
	if _, err := codec.Receive(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown Receive() error = %v", err)
	}
}

func TestHandshakeEnforcesPreparedCommitRunningOrder(t *testing.T) {
	parent, child := net.Pipe()
	defer parent.Close()
	defer child.Close()
	parentCodec, _ := NewCodec(parent, parent)
	childCodec, _ := NewCodec(child, child)
	supervisor, _ := NewSupervisorHandshake(parentCodec)
	worker, _ := NewWorkerHandshake(childCodec)

	workerDone := make(chan error, 1)
	go func() {
		bootstrap, err := worker.AwaitBootstrap()
		if err != nil {
			workerDone <- err
			return
		}
		if bootstrap.ConfigDigest != testDigest {
			workerDone <- errors.New("unexpected digest")
			return
		}
		if err := worker.Prepared(Prepared{PID: 99, UID: 501, GID: 20}); err != nil {
			workerDone <- err
			return
		}
		if _, err := worker.AwaitCommit(); err != nil {
			workerDone <- err
			return
		}
		workerDone <- worker.Running()
	}()

	if err := supervisor.Commit(); err == nil {
		t.Fatal("commit succeeded before worker prepared")
	}
	if err := supervisor.Bootstrap(validBootstrap()); err != nil {
		t.Fatal(err)
	}
	prepared, err := supervisor.AwaitPrepared()
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PID != 99 {
		t.Fatalf("prepared = %+v", prepared)
	}
	if _, err := supervisor.AwaitRunning(); err == nil {
		t.Fatal("running succeeded before supervisor commit")
	}
	if err := supervisor.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.AwaitRunning(); err != nil {
		t.Fatal(err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeRejectsDigestMismatch(t *testing.T) {
	var wire bytes.Buffer
	codec, _ := NewCodec(&wire, &wire)
	worker, _ := NewWorkerHandshake(codec)
	if err := codec.Send(KindBootstrap, 0, validBootstrap()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.AwaitBootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := worker.Prepared(Prepared{PID: 1, UID: 2, GID: 3}); err != nil {
		t.Fatal(err)
	}
	if message, err := codec.Receive(); err != nil || message.Kind != KindPrepared {
		t.Fatalf("consume prepared message = %+v, %v", message, err)
	}
	if err := codec.Send(KindCommit, 0, Commit{ConfigDigest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.AwaitCommit(); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("AwaitCommit() error = %v", err)
	}
}

func TestBootstrapValidation(t *testing.T) {
	value := validBootstrap()
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.DNSListen = "0.0.0.0:53"
	value.StatusSocket = "relative.sock"
	value.ConfigDigest = "bad"
	if err := value.Validate(); err == nil {
		t.Fatal("unsafe bootstrap was accepted")
	}
}

func TestBootstrapValidationRejectsPayloadDigestMismatch(t *testing.T) {
	value := validBootstrap()
	value.Config = []byte("version: 2\n")
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "payload digest") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestReloadValidation(t *testing.T) {
	value := Reload{
		Config: []byte("version: 1\n"), ConfigDigest: testDigest,
		InterfaceDNS: map[string][]netip.AddrPort{"en7": {netip.MustParseAddrPort("192.168.100.51:53")}},
	}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Config = nil
	value.ConfigDigest = "bad"
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "reload config is empty") || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("unsafe reload error = %v", err)
	}
}

func TestInterfaceDNSValidationRejectsRecursiveAndMalformedValues(t *testing.T) {
	value := validBootstrap()
	value.InterfaceDNS = map[string][]netip.AddrPort{
		"-bad": {netip.MustParseAddrPort("127.0.0.1:53")},
	}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "interface DNS") {
		t.Fatalf("unsafe interface DNS error = %v", err)
	}
}
