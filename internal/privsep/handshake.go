package privsep

import (
	"errors"
	"fmt"
)

type supervisorState uint8

const (
	supervisorInitial supervisorState = iota
	supervisorBootstrapped
	supervisorPrepared
	supervisorCommitted
	supervisorRunning
)

// SupervisorHandshake enforces the startup transaction boundary. Host routes
// and system DNS may be committed only after AwaitPrepared succeeds, and the
// service may be published as running only after AwaitRunning succeeds.
type SupervisorHandshake struct {
	codec  *Codec
	state  supervisorState
	digest string
}

func NewSupervisorHandshake(codec *Codec) (*SupervisorHandshake, error) {
	if codec == nil {
		return nil, errors.New("supervisor handshake requires a protocol codec")
	}
	return &SupervisorHandshake{codec: codec}, nil
}

func (handshake *SupervisorHandshake) Bootstrap(payload Bootstrap) error {
	if handshake.state != supervisorInitial {
		return handshake.invalid("bootstrap")
	}
	if err := payload.Validate(); err != nil {
		return err
	}
	if err := handshake.codec.Send(KindBootstrap, 0, payload); err != nil {
		return err
	}
	handshake.digest = payload.ConfigDigest
	handshake.state = supervisorBootstrapped
	return nil
}

func (handshake *SupervisorHandshake) AwaitPrepared() (Prepared, error) {
	if handshake.state != supervisorBootstrapped {
		return Prepared{}, handshake.invalid("await prepared")
	}
	payload, _, err := ReceiveKind[Prepared](handshake.codec, KindPrepared)
	if err != nil {
		return Prepared{}, err
	}
	if err := payload.Validate(); err != nil {
		return Prepared{}, err
	}
	handshake.state = supervisorPrepared
	return payload, nil
}

func (handshake *SupervisorHandshake) Commit() error {
	if handshake.state != supervisorPrepared {
		return handshake.invalid("commit")
	}
	if err := handshake.codec.Send(KindCommit, 0, Commit{ConfigDigest: handshake.digest}); err != nil {
		return err
	}
	handshake.state = supervisorCommitted
	return nil
}

func (handshake *SupervisorHandshake) AwaitRunning() (Running, error) {
	if handshake.state != supervisorCommitted {
		return Running{}, handshake.invalid("await running")
	}
	payload, _, err := ReceiveKind[Running](handshake.codec, KindRunning)
	if err != nil {
		return Running{}, err
	}
	if payload.ConfigDigest != handshake.digest {
		return Running{}, fmt.Errorf("worker running digest=%q, want %q", payload.ConfigDigest, handshake.digest)
	}
	handshake.state = supervisorRunning
	return payload, nil
}

func (handshake *SupervisorHandshake) invalid(operation string) error {
	return fmt.Errorf("cannot %s in supervisor handshake state %d", operation, handshake.state)
}

type workerState uint8

const (
	workerInitial workerState = iota
	workerBootstrapped
	workerPrepared
	workerCommitted
	workerRunning
)

type WorkerHandshake struct {
	codec  *Codec
	state  workerState
	digest string
}

func NewWorkerHandshake(codec *Codec) (*WorkerHandshake, error) {
	if codec == nil {
		return nil, errors.New("worker handshake requires a protocol codec")
	}
	return &WorkerHandshake{codec: codec}, nil
}

func (handshake *WorkerHandshake) AwaitBootstrap() (Bootstrap, error) {
	if handshake.state != workerInitial {
		return Bootstrap{}, handshake.invalid("await bootstrap")
	}
	payload, _, err := ReceiveKind[Bootstrap](handshake.codec, KindBootstrap)
	if err != nil {
		return Bootstrap{}, err
	}
	if err := payload.Validate(); err != nil {
		return Bootstrap{}, err
	}
	handshake.digest = payload.ConfigDigest
	handshake.state = workerBootstrapped
	return payload, nil
}

func (handshake *WorkerHandshake) Prepared(payload Prepared) error {
	if handshake.state != workerBootstrapped {
		return handshake.invalid("report prepared")
	}
	if err := payload.Validate(); err != nil {
		return err
	}
	if err := handshake.codec.Send(KindPrepared, 0, payload); err != nil {
		return err
	}
	handshake.state = workerPrepared
	return nil
}

func (handshake *WorkerHandshake) AwaitCommit() (Commit, error) {
	if handshake.state != workerPrepared {
		return Commit{}, handshake.invalid("await commit")
	}
	payload, _, err := ReceiveKind[Commit](handshake.codec, KindCommit)
	if err != nil {
		return Commit{}, err
	}
	if payload.ConfigDigest != handshake.digest {
		return Commit{}, fmt.Errorf("supervisor commit digest=%q, want %q", payload.ConfigDigest, handshake.digest)
	}
	handshake.state = workerCommitted
	return payload, nil
}

func (handshake *WorkerHandshake) Running() error {
	if handshake.state != workerCommitted {
		return handshake.invalid("report running")
	}
	if err := handshake.codec.Send(KindRunning, 0, Running{ConfigDigest: handshake.digest}); err != nil {
		return err
	}
	handshake.state = workerRunning
	return nil
}

func (handshake *WorkerHandshake) invalid(operation string) error {
	return fmt.Errorf("cannot %s in worker handshake state %d", operation, handshake.state)
}
