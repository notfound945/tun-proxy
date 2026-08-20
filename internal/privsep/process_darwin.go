//go:build darwin

package privsep

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type fileDuplicator interface {
	File() (*os.File, error)
}

// Handoff owns the private control channel and the child-side descriptor
// duplicates. The TUN file in ExtraFiles is borrowed from the supervisor's
// device and remains owned by that device.
type Handoff struct {
	Control net.Conn
	extra   []*os.File
	owned   []*os.File
}

// PrepareHandoff creates the private supervisor/worker channel and orders the
// worker descriptors so exec installs them at ControlFD through TCPDNSFD.
func PrepareHandoff(tunFile *os.File, udpDNS, tcpDNS fileDuplicator) (*Handoff, error) {
	if tunFile == nil {
		return nil, errors.New("prepare worker handoff: nil TUN file")
	}
	if udpDNS == nil || tcpDNS == nil {
		return nil, errors.New("prepare worker handoff: both DNS listeners are required")
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create private worker control socketpair: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	parentFile := os.NewFile(uintptr(fds[0]), "tun-proxy-control-parent")
	childFile := os.NewFile(uintptr(fds[1]), "tun-proxy-control-child")
	parent, err := openControlFile(parentFile, "private supervisor")
	if err != nil {
		_ = childFile.Close()
		return nil, err
	}
	fail := func(failure error, owned ...*os.File) (*Handoff, error) {
		for _, file := range owned {
			if file != nil {
				_ = file.Close()
			}
		}
		_ = childFile.Close()
		_ = parent.Close()
		return nil, failure
	}
	udpFile, err := udpDNS.File()
	if err != nil {
		return fail(fmt.Errorf("duplicate UDP DNS descriptor: %w", err))
	}
	tcpFile, err := tcpDNS.File()
	if err != nil {
		return fail(fmt.Errorf("duplicate TCP DNS descriptor: %w", err), udpFile)
	}
	return &Handoff{
		Control: parent,
		extra:   []*os.File{childFile, tunFile, udpFile, tcpFile},
		owned:   []*os.File{childFile, udpFile, tcpFile},
	}, nil
}

func (handoff *Handoff) ExtraFiles() []*os.File {
	if handoff == nil {
		return nil
	}
	return append([]*os.File(nil), handoff.extra...)
}

// CloseChildFiles closes the supervisor's child-side duplicates after
// exec.Cmd.Start has copied them into the worker. The control parent stays
// open for the protocol session.
func (handoff *Handoff) CloseChildFiles() error {
	if handoff == nil {
		return nil
	}
	var failures []error
	for _, file := range handoff.owned {
		if file != nil {
			failures = append(failures, file.Close())
		}
	}
	handoff.owned = nil
	handoff.extra = nil
	return errors.Join(failures...)
}

func (handoff *Handoff) Close() error {
	if handoff == nil {
		return nil
	}
	childErr := handoff.CloseChildFiles()
	var controlErr error
	if handoff.Control != nil {
		controlErr = handoff.Control.Close()
		handoff.Control = nil
	}
	return errors.Join(childErr, controlErr)
}

// WorkerCommand constructs the only supported production worker exec shape.
// No service paths or identity values are accepted as child command-line
// arguments; the worker re-resolves and validates the dedicated identity.
func WorkerCommand(executable string, identity Identity, handoff *Handoff) (*exec.Cmd, error) {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return nil, fmt.Errorf("worker executable must be a clean absolute path: %q", executable)
	}
	if identity.User != ProductionUser || identity.Group != ProductionGroup || identity.Home != ProductionHome || identity.UID == 0 || identity.GID == 0 {
		return nil, fmt.Errorf("unsafe production worker identity: %+v", identity)
	}
	if handoff == nil || handoff.Control == nil || len(handoff.extra) != 4 {
		return nil, errors.New("complete worker descriptor handoff is required")
	}
	command := exec.Command(executable, "_service-worker")
	command.Dir = ProductionHome
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "LANG=C"}
	command.ExtraFiles = handoff.ExtraFiles()
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: identity.UID, Gid: identity.GID, Groups: []uint32{identity.GID},
	}}
	return command, nil
}
