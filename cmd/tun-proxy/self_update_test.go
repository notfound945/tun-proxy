package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

type selfUpdateProcessCall struct {
	executable string
	args       []string
	stdin      string
}

func TestRunReleaseSelfUpdateDownloadsThenExecutesScript(t *testing.T) {
	const downloaded = "#!/usr/bin/env bash\nprintf 'updated\\n'\n"
	var calls []selfUpdateProcessCall
	var output bytes.Buffer
	var errorOutput bytes.Buffer
	runner := func(_ context.Context, executable string, args []string, stdin io.Reader, stdout, _ io.Writer) error {
		call := selfUpdateProcessCall{executable: executable, args: slices.Clone(args)}
		if stdin != nil {
			contents, err := io.ReadAll(stdin)
			if err != nil {
				return err
			}
			call.stdin = string(contents)
		}
		calls = append(calls, call)
		switch executable {
		case "/usr/bin/curl":
			_, err := io.WriteString(stdout, downloaded)
			return err
		case "/bin/bash":
			_, err := io.WriteString(stdout, "updated\n")
			return err
		default:
			return errors.New("unexpected executable")
		}
	}

	if err := runReleaseSelfUpdate(t.Context(), &output, &errorOutput, runner); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want download and bash execution", calls)
	}
	if calls[0].executable != "/usr/bin/curl" || !slices.Equal(calls[0].args, []string{"-fsSL", releaseUpdateScriptURL}) || calls[0].stdin != "" {
		t.Fatalf("download call = %+v", calls[0])
	}
	if calls[1].executable != "/bin/bash" || len(calls[1].args) != 0 || calls[1].stdin != downloaded {
		t.Fatalf("bash call = %+v", calls[1])
	}
	if output.String() != "updated\n" || errorOutput.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", output.String(), errorOutput.String())
	}
}

func TestRunReleaseSelfUpdateRejectsDownloadFailure(t *testing.T) {
	calls := 0
	runner := func(_ context.Context, executable string, _ []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
		calls++
		if executable != "/usr/bin/curl" {
			t.Fatalf("unexpected executable after download failure: %s", executable)
		}
		return errors.New("network unavailable")
	}

	err := runReleaseSelfUpdate(t.Context(), io.Discard, io.Discard, runner)
	if err == nil || !strings.Contains(err.Error(), "download release update script") {
		t.Fatalf("runReleaseSelfUpdate() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestRunReleaseSelfUpdateRejectsEmptyScript(t *testing.T) {
	calls := 0
	runner := func(_ context.Context, executable string, _ []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
		calls++
		if executable != "/usr/bin/curl" {
			t.Fatalf("unexpected executable for empty script: %s", executable)
		}
		return nil
	}

	err := runReleaseSelfUpdate(t.Context(), io.Discard, io.Discard, runner)
	if err == nil || !strings.Contains(err.Error(), "script is empty") {
		t.Fatalf("runReleaseSelfUpdate() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestRunReleaseSelfUpdateReportsBashFailure(t *testing.T) {
	runner := func(_ context.Context, executable string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		if executable == "/usr/bin/curl" {
			_, err := io.WriteString(stdout, "exit 1\n")
			return err
		}
		if executable == "/bin/bash" {
			return errors.New("exit status 1")
		}
		return errors.New("unexpected executable")
	}

	err := runReleaseSelfUpdate(t.Context(), io.Discard, io.Discard, runner)
	if err == nil || !strings.Contains(err.Error(), "run release update script") {
		t.Fatalf("runReleaseSelfUpdate() error = %v", err)
	}
}

func TestSelfUpdateRejectsArgumentsBeforeDownload(t *testing.T) {
	err := runWithVersionOutput([]string{"self-update", "unexpected"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "self-update does not accept arguments") {
		t.Fatalf("runWithVersionOutput(self-update) error = %v", err)
	}
}
