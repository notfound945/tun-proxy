package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"

	"golang.org/x/sys/unix"
)

const releaseUpdateScriptURL = "https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh"

type selfUpdateProcessRunner func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error

func selfUpdateCommand(args []string) error {
	if hasOnlyHelpArgument(args) {
		return helpCommand([]string{"self-update"})
	}
	if len(args) != 0 {
		return errors.New("self-update does not accept arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	return runReleaseSelfUpdate(ctx, os.Stdout, os.Stderr, executeSelfUpdateProcess)
}

func runReleaseSelfUpdate(ctx context.Context, stdout, stderr io.Writer, runner selfUpdateProcessRunner) error {
	var script bytes.Buffer
	if err := runner(ctx, "/usr/bin/curl", []string{"-fsSL", releaseUpdateScriptURL}, nil, &script, stderr); err != nil {
		return fmt.Errorf("download release update script: %w", err)
	}
	if script.Len() == 0 {
		return errors.New("downloaded release update script is empty")
	}
	if err := runner(ctx, "/bin/bash", nil, &script, stdout, stderr); err != nil {
		return fmt.Errorf("run release update script: %w", err)
	}
	return nil
}

func executeSelfUpdateProcess(ctx context.Context, executable string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}
