//go:build darwin

package privsep

import (
	"net"
	"os"
	"testing"
)

func TestPrepareHandoffAndWorkerCommand(t *testing.T) {
	tunFile, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer tunFile.Close() //nolint:errcheck // Best-effort cleanup.
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close() //nolint:errcheck // Best-effort cleanup.
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close() //nolint:errcheck // Best-effort cleanup.

	handoff, err := PrepareHandoff(tunFile, udp, tcp)
	if err != nil {
		t.Fatal(err)
	}
	defer handoff.Close() //nolint:errcheck // Best-effort cleanup.
	files := handoff.ExtraFiles()
	if len(files) != 4 || files[1] != tunFile {
		t.Fatalf("handoff files = %v", files)
	}
	identity := Identity{User: ProductionUser, Group: ProductionGroup, UID: 499, GID: 499, Home: ProductionHome}
	command, err := WorkerCommand("/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy", identity, handoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(command.Args) != 2 || command.Args[1] != "_service-worker" {
		t.Fatalf("worker args = %v", command.Args)
	}
	if command.Dir != ProductionHome || len(command.ExtraFiles) != 4 {
		t.Fatalf("worker command = dir=%q files=%d", command.Dir, len(command.ExtraFiles))
	}
	credential := command.SysProcAttr.Credential
	if credential.Uid != identity.UID || credential.Gid != identity.GID || len(credential.Groups) != 1 || credential.Groups[0] != identity.GID {
		t.Fatalf("worker credential = %+v", credential)
	}
}

func TestWorkerCommandRejectsUnsafeInputs(t *testing.T) {
	if _, err := WorkerCommand("relative", Identity{}, nil); err == nil {
		t.Fatal("relative executable was accepted")
	}
	if _, err := PrepareHandoff(nil, nil, nil); err == nil {
		t.Fatal("empty descriptor handoff was accepted")
	}
}
