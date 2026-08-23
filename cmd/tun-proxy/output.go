package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/apperror"
)

type outputMode string

const (
	outputText outputMode = "text"
	outputJSON outputMode = "json"
)

type successEnvelope struct {
	OK          bool            `json:"ok"`
	Output      string          `json:"output,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Diagnostics string          `json:"diagnostics,omitempty"`
}

func executeCLI(args []string, stdout, stderr io.Writer) int {
	return executeCLIWithRunner(args, stdout, stderr, run)
}

func executeCLIWithRunner(args []string, stdout, stderr io.Writer, runCommand func([]string) error) int {
	mode, commandArgs, parseErr := parseGlobalOutput(args)
	managedProcess := isManagedServiceProcess(commandArgs)
	if managedProcess {
		mode = outputText
	}
	if parseErr != nil {
		if managedProcess {
			renderManagedServiceError(parseErr, stderr)
			return 1
		}
		classified := classifyCLIError(commandArgs, parseErr)
		renderCLIError(mode, classified, stdout, stderr)
		return apperror.ExitCodeOf(classified)
	}
	if mode == outputText {
		err := runCommand(commandArgs)
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if err != nil {
			if managedProcess {
				renderManagedServiceError(err, stderr)
				return 1
			}
			err = classifyCLIError(commandArgs, err)
			renderCLIError(mode, err, stdout, stderr)
			return apperror.ExitCodeOf(err)
		}
		return 0
	}

	capturedOut, capturedErr, err := captureCommandOutput(func() error {
		return runCommand(withGlobalJSONAlias(commandArgs))
	})
	if errors.Is(err, flag.ErrHelp) {
		err = nil
	}
	if err != nil {
		err = classifyCLIError(commandArgs, err)
		renderCLIError(mode, err, stdout, stderr)
		return apperror.ExitCodeOf(err)
	}
	renderJSONSuccess(stdout, capturedOut, capturedErr)
	return 0
}

func parseGlobalOutput(args []string) (outputMode, []string, error) {
	mode := outputText
	remaining := append([]string(nil), args...)
	for len(remaining) > 0 {
		argument := remaining[0]
		if argument == "--" {
			return mode, remaining[1:], nil
		}
		var value string
		switch {
		case argument == "--output" || argument == "-output":
			if len(remaining) < 2 {
				return mode, nil, apperror.New(apperror.CodeUsageError, "cli", "--output requires text or json")
			}
			value = remaining[1]
			remaining = remaining[2:]
		case strings.HasPrefix(argument, "--output="):
			value = strings.TrimPrefix(argument, "--output=")
			remaining = remaining[1:]
		case strings.HasPrefix(argument, "-output="):
			value = strings.TrimPrefix(argument, "-output=")
			remaining = remaining[1:]
		default:
			return mode, remaining, nil
		}
		switch outputMode(value) {
		case outputText, outputJSON:
			mode = outputMode(value)
		default:
			return mode, remaining, apperror.Wrap(apperror.CodeUsageError, "cli", "--output must be text or json", fmt.Errorf("got %q", value))
		}
	}
	return mode, remaining, nil
}

func withGlobalJSONAlias(args []string) []string {
	result := append([]string(nil), args...)
	insert := func(index int) []string {
		result = append(result, "")
		copy(result[index+1:], result[index:])
		result[index] = "-json=true"
		return result
	}
	if len(result) == 0 {
		return result
	}
	switch result[0] {
	case "status", "explain", "diagnose":
		return insert(1)
	case "config":
		if len(result) > 1 && result[1] == "validate" {
			return insert(2)
		}
	case "service":
		if len(result) > 1 && result[1] == "status" {
			return insert(2)
		}
	}
	return result
}

func captureCommandOutput(runCommand func() error) ([]byte, []byte, error) {
	stdoutFile, err := os.CreateTemp("", "tun-proxy-stdout-*")
	if err != nil {
		return nil, nil, err
	}
	stdoutName := stdoutFile.Name()
	defer os.Remove(stdoutName) //nolint:errcheck // Best-effort temporary cleanup.
	stderrFile, err := os.CreateTemp("", "tun-proxy-stderr-*")
	if err != nil {
		_ = stdoutFile.Close()
		return nil, nil, err
	}
	stderrName := stderrFile.Name()
	defer os.Remove(stderrName) //nolint:errcheck // Best-effort temporary cleanup.

	originalStdout, originalStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutFile, stderrFile
	defer func() {
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
	}()
	commandErr := func() error {
		defer func() {
			os.Stdout, os.Stderr = originalStdout, originalStderr
		}()
		return runCommand()
	}()

	closeErr := errors.Join(stdoutFile.Close(), stderrFile.Close())
	stdoutBytes, stdoutErr := os.ReadFile(stdoutName)
	stderrBytes, stderrErr := os.ReadFile(stderrName)
	captureErr := errors.Join(closeErr, stdoutErr, stderrErr)
	if captureErr == nil {
		return stdoutBytes, stderrBytes, commandErr
	}
	return stdoutBytes, stderrBytes, errors.Join(commandErr, captureErr)
}

func renderManagedServiceError(err error, stderr io.Writer) {
	writeManagedServiceLog(stderr, time.Now(), "ERROR "+err.Error())
}

func renderCLIError(mode outputMode, err error, stdout, stderr io.Writer) {
	if mode == outputJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(apperror.EnvelopeOf(err))
		return
	}
	fmt.Fprintln(stderr, "error:", err)
	info := apperror.InfoOf(err)
	for _, key := range []string{"holder_operation", "holder_operation_id", "holder_pid", "started_at", "operation_id", "reload_request_id", "rollback_of", "phase"} {
		if value, ok := info.Details[key]; ok {
			fmt.Fprintf(stderr, "  %s: %v\n", strings.ReplaceAll(key, "_", "-"), value)
		}
	}
}

func renderJSONSuccess(writer io.Writer, stdout, stderr []byte) {
	trimmed := bytes.TrimSpace(stdout)
	if len(stderr) == 0 && len(trimmed) > 0 && json.Valid(trimmed) {
		_, _ = writer.Write(trimmed)
		_, _ = io.WriteString(writer, "\n")
		return
	}
	envelope := successEnvelope{OK: true, Output: string(stdout), Diagnostics: string(stderr)}
	if len(trimmed) > 0 && json.Valid(trimmed) {
		envelope.Result = append(json.RawMessage(nil), trimmed...)
		envelope.Output = ""
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(envelope)
}

func classifyCLIError(args []string, err error) error {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	var typed *apperror.Error
	if errors.As(err, &typed) && typed != nil {
		return err
	}
	operation := commandOperation(args)
	message := strings.ToLower(err.Error())
	wrap := func(code apperror.Code, summary string) error {
		return apperror.Wrap(code, operation, summary, err)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		switch operation {
		case "service.reload":
			return wrap(apperror.CodeReloadTimeout, "configuration reload did not complete before the timeout")
		case "service.start":
			return wrap(apperror.CodeServiceStartTimeout, "service did not become ready before the startup timeout")
		case "service.stop":
			return wrap(apperror.CodeServiceStopTimeout, "service did not stop before the timeout")
		default:
			return wrap(apperror.CodeServiceUnreachable, "operation timed out")
		}
	case strings.Contains(message, "root privileges are required"):
		return wrap(apperror.CodeRootRequired, "root privileges are required; run with sudo")
	case strings.Contains(message, "not installed") || strings.Contains(message, "not completely installed"):
		return wrap(apperror.CodeServiceNotInstalled, "tun-proxy service is not installed")
	case strings.Contains(message, "not running"):
		return wrap(apperror.CodeServiceNotRunning, "tun-proxy service is not running")
	case strings.Contains(message, "unsafe") || strings.Contains(message, "symlink") || strings.Contains(message, "owned by uid") || strings.Contains(message, "permissions"):
		return wrap(apperror.CodeUnsafeFile, "a required service file or socket is unsafe")
	case isUsageMessage(message):
		return wrap(apperror.CodeUsageError, "invalid command usage")
	case operation == "config.validate" || operation == "check":
		return wrap(apperror.CodeConfigInvalid, "configuration is invalid")
	case operation == "service.reload":
		return wrap(apperror.CodeReloadRejected, "configuration reload was rejected")
	default:
		return wrap(apperror.CodeInternalError, "tun-proxy command failed")
	}
}

func isUsageMessage(message string) bool {
	for _, fragment := range []string{
		"unknown command", "unknown service command", "unknown config command", "unknown help topic",
		"command is required", "does not accept arguments", "unexpected arguments", "flag provided but not defined",
		"invalid value", "must be positive", "choose exactly one", "requires -", "must be true or false",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func commandOperation(args []string) string {
	if len(args) == 0 {
		return "cli"
	}
	if args[0] == "service" && len(args) > 1 {
		return "service." + args[1]
	}
	if args[0] == "config" && len(args) > 1 {
		return "config." + args[1]
	}
	return args[0]
}
