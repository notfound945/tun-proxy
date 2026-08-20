package system

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const commandOutputLimit = 64 << 10

var allowedExecutables = map[string]struct{}{
	"/sbin/ifconfig":         {},
	"/sbin/route":            {},
	"/usr/sbin/ipconfig":     {},
	"/usr/sbin/networksetup": {},
	"/usr/sbin/scutil":       {},
}

type CommandRunner interface {
	Run(ctx context.Context, executable string, args ...string) ([]byte, error)
}

type NativeCommandRunner struct{}

func (NativeCommandRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	return RunCommand(ctx, executable, args...)
}

func RunCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	if _, allowed := allowedExecutables[executable]; !allowed {
		return nil, fmt.Errorf("system executable %q is not allowed", executable)
	}
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	command := exec.CommandContext(commandCtx, executable, args...)
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "LANG=C"}
	output := newLimitedBuffer(commandOutputLimit)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if commandCtx.Err() != nil {
			return nil, fmt.Errorf("run %s: %w", executable, commandCtx.Err())
		}
		return nil, fmt.Errorf("run %s %s: %w: %s", executable, strings.Join(args, " "), err, strings.TrimSpace(output.String()))
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{remaining: limit} }

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	if len(data) > buffer.remaining {
		data = data[:buffer.remaining]
	}
	if len(data) > 0 {
		_, _ = buffer.buffer.Write(data)
		buffer.remaining -= len(data)
	}
	return originalLength, nil
}

func (buffer *limitedBuffer) Bytes() []byte  { return buffer.buffer.Bytes() }
func (buffer *limitedBuffer) String() string { return buffer.buffer.String() }
