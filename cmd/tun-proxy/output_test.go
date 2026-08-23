package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/apperror"
)

func TestParseGlobalOutput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		mode outputMode
		want []string
		code apperror.Code
	}{
		{name: "default", args: []string{"status"}, mode: outputText, want: []string{"status"}},
		{name: "json equals", args: []string{"--output=json", "status"}, mode: outputJSON, want: []string{"status"}},
		{name: "json separate", args: []string{"--output", "json", "service", "status"}, mode: outputJSON, want: []string{"service", "status"}},
		{name: "last global wins", args: []string{"--output=json", "--output=text", "status"}, mode: outputText, want: []string{"status"}},
		{name: "stops at command", args: []string{"status", "--output=json"}, mode: outputText, want: []string{"status", "--output=json"}},
		{name: "invalid", args: []string{"--output=yaml", "status"}, mode: outputText, want: []string{"status"}, code: apperror.CodeUsageError},
		{name: "missing", args: []string{"--output"}, mode: outputText, code: apperror.CodeUsageError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode, got, err := parseGlobalOutput(test.args)
			if mode != test.mode || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseGlobalOutput() = %q %#v, want %q %#v", mode, got, test.mode, test.want)
			}
			if test.code == "" && err != nil {
				t.Fatal(err)
			}
			if test.code != "" && apperror.CodeOf(err) != test.code {
				t.Fatalf("error = %v, code = %s", err, apperror.CodeOf(err))
			}
		})
	}
}

func TestWithGlobalJSONAlias(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{"status", "-state", "x"}, []string{"status", "-json=true", "-state", "x"}},
		{[]string{"service", "status"}, []string{"service", "status", "-json=true"}},
		{[]string{"config", "validate"}, []string{"config", "validate", "-json=true"}},
		{[]string{"service", "reload"}, []string{"service", "reload"}},
	}
	for _, test := range tests {
		if got := withGlobalJSONAlias(test.in); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("withGlobalJSONAlias(%#v) = %#v, want %#v", test.in, got, test.want)
		}
	}
}

func TestExecuteCLIJSONFailureIsSingleDocument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := executeCLIWithRunner([]string{"--output=json", "service", "reload"}, &stdout, &stderr, func(args []string) error {
		if _, err := fmt.Fprintln(os.Stdout, "partial command output"); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stderr, "partial diagnostic"); err != nil {
			t.Fatal(err)
		}
		return apperror.New(apperror.CodeReloadTimeout, "service.reload", "reload timed out")
	})
	if code != 6 {
		t.Fatalf("exit code = %d, want 6", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(stdout.String(), "partial") {
		t.Fatalf("stdout leaked partial output: %q", stdout.String())
	}
	var envelope apperror.Envelope
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v; output=%q", err, stdout.String())
	}
	if decoder.Decode(&struct{}{}) == nil {
		t.Fatalf("stdout contains more than one JSON document: %q", stdout.String())
	}
	if envelope.OK || envelope.Error.Code != apperror.CodeReloadTimeout {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestExecuteCLIJSONSuccessUsesExistingJSONCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := executeCLIWithRunner([]string{"--output=json", "service", "status"}, &stdout, &stderr, func(args []string) error {
		want := []string{"service", "status", "-json=true"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		if _, err := fmt.Fprintln(os.Stdout, `{"installed":true}`); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if code != 0 || stderr.Len() != 0 || strings.TrimSpace(stdout.String()) != `{"installed":true}` {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExecuteCLIJSONWrapsTextSuccessAndDiagnostics(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := executeCLIWithRunner([]string{"--output=json", "version"}, &stdout, &stderr, func(args []string) error {
		if _, err := fmt.Fprintln(os.Stdout, "tun-proxy test"); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stderr, "warning"); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var envelope successEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Output != "tun-proxy test\n" || envelope.Diagnostics != "warning\n" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestExecuteCLITextErrorRemainsReadable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := executeCLIWithRunner([]string{"service", "reload"}, &stdout, &stderr, func([]string) error {
		return apperror.New(apperror.CodeServiceNotRunning, "service.reload", "service is not running").WithDetails(map[string]any{"phase": "stopped"})
	})
	if code != 5 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "error: service is not running") || !strings.Contains(stderr.String(), "phase: stopped") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExecuteCLIManagedProcessIgnoresGlobalJSONMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := executeCLIWithRunner([]string{"--output=json", "_service-run"}, &stdout, &stderr, func(args []string) error {
		if !reflect.DeepEqual(args, []string{"_service-run"}) {
			t.Fatalf("args = %#v", args)
		}
		return errors.New("managed failure")
	})
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), " ERROR managed failure") || !strings.HasSuffix(stderr.String(), "\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExecuteCLIInvalidOutputIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false
	code := executeCLIWithRunner([]string{"--output=yaml", "status"}, &stdout, &stderr, func([]string) error {
		called = true
		return nil
	})
	if called || code != 2 || !strings.Contains(stderr.String(), "--output must be text or json") {
		t.Fatalf("called=%t code=%d stdout=%q stderr=%q", called, code, stdout.String(), stderr.String())
	}
}

func TestCaptureCommandOutputRestoresDescriptorsAfterPanic(t *testing.T) {
	originalStdout, originalStderr := os.Stdout, os.Stderr
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("captureCommandOutput did not propagate panic")
		}
		if os.Stdout != originalStdout || os.Stderr != originalStderr {
			t.Fatal("stdout/stderr were not restored after panic")
		}
	}()
	_, _, _ = captureCommandOutput(func() error { panic("boom") })
}

func TestClassifyCLIErrorUsageAndConfig(t *testing.T) {
	if got := classifyCLIError([]string{"status"}, flag.ErrHelp); !errors.Is(got, flag.ErrHelp) {
		t.Fatalf("help error = %v", got)
	}
	usage := classifyCLIError([]string{"service", "reload"}, errors.New("service reload timeout must be positive"))
	if got := apperror.CodeOf(usage); got != apperror.CodeUsageError {
		t.Fatalf("usage code = %s", got)
	}
	invalid := classifyCLIError([]string{"config", "validate"}, errors.New("bad yaml"))
	if got := apperror.CodeOf(invalid); got != apperror.CodeConfigInvalid {
		t.Fatalf("config code = %s", got)
	}
}

func TestClassifyCLIErrorPreservesTypedInternalError(t *testing.T) {
	original := apperror.New(apperror.CodeInternalError, "service.reload", "state persistence failed")
	got := classifyCLIError([]string{"service", "reload"}, original)
	if got != original {
		t.Fatalf("classifyCLIError() replaced typed error: got %v, want %v", got, original)
	}
	if code := apperror.CodeOf(got); code != apperror.CodeInternalError {
		t.Fatalf("code = %s, want %s", code, apperror.CodeInternalError)
	}
}
