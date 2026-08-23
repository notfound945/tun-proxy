package apperror

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestErrorPreservesCauseChain(t *testing.T) {
	sentinel := errors.New("sentinel")
	err := Wrap(CodeReloadRejected, "service.reload", "reload rejected", sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is did not find wrapped cause")
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeReloadRejected {
		t.Fatalf("errors.As = %#v", typed)
	}
	if !errors.Is(errors.Unwrap(err), sentinel) {
		t.Fatalf("Unwrap() = %v", errors.Unwrap(err))
	}
}

func TestCodeExitAndRetryableMappings(t *testing.T) {
	tests := []struct {
		code      Code
		exit      int
		retryable bool
	}{
		{CodeUsageError, 2, false},
		{CodeConfigInvalid, 3, false},
		{CodeConfigRestartRequired, 3, false},
		{CodeRootRequired, 4, false},
		{CodeUnsafeFile, 4, false},
		{CodeServiceNotInstalled, 5, false},
		{CodeServiceNotRunning, 5, true},
		{CodeReloadRejected, 5, false},
		{CodeReloadRequestMismatch, 5, false},
		{CodeReloadDigestMismatch, 5, false},
		{CodeServiceStartTimeout, 6, true},
		{CodeServiceStopTimeout, 6, true},
		{CodeServiceUnreachable, 6, true},
		{CodeReloadTimeout, 6, true},
		{CodeRollbackIncomplete, 7, false},
		{CodeServiceOperationBusy, 8, true},
		{CodeReloadRequestConflict, 8, false},
		{CodeServiceProtocolTooOld, 9, false},
		{CodeInternalError, 1, false},
	}
	for _, test := range tests {
		t.Run(string(test.code), func(t *testing.T) {
			err := New(test.code, "test", "failed")
			if got := CodeOf(err); got != test.code {
				t.Fatalf("CodeOf() = %s, want %s", got, test.code)
			}
			if got := ExitCodeOf(err); got != test.exit {
				t.Fatalf("ExitCodeOf() = %d, want %d", got, test.exit)
			}
			if err.Retryable != test.retryable || Retryable(test.code) != test.retryable {
				t.Fatalf("retryable = %t/%t, want %t", err.Retryable, Retryable(test.code), test.retryable)
			}
		})
	}
	if got := CodeOf(errors.New("plain")); got != CodeInternalError {
		t.Fatalf("plain CodeOf() = %s", got)
	}
	if got := ExitCode(Code("UNKNOWN")); got != 1 {
		t.Fatalf("unknown ExitCode() = %d", got)
	}
}

func TestInfoOfExpandsJoinedCauses(t *testing.T) {
	first := New(CodeReloadRejected, "service.reload", "apply failed")
	second := New(CodeServiceUnreachable, "service.reload", "rollback failed")
	info := InfoOf(errors.Join(first, second))
	if info.Code != CodeInternalError || len(info.Causes) != 2 {
		t.Fatalf("InfoOf(join) = %+v", info)
	}
	if info.Causes[0].Code != CodeReloadRejected || info.Causes[1].Code != CodeServiceUnreachable {
		t.Fatalf("cause codes = %+v", info.Causes)
	}
	if got := CodeOf(errors.Join(first, second)); got != CodeInternalError {
		t.Fatalf("CodeOf(join) = %s, want %s", got, CodeInternalError)
	}
}

func TestRollbackIncompleteEnvelopeExpandsTypedCauses(t *testing.T) {
	apply := New(CodeReloadRejected, "service.reload", "configuration reload failed")
	rollback := New(CodeReloadTimeout, "service.reload", "rollback reload timed out")
	err := Wrap(
		CodeRollbackIncomplete,
		"service.reload",
		"configuration reload failed and rollback was incomplete",
		errors.Join(apply, rollback),
	).WithDetails(map[string]any{
		"operation_id":      "fedcba9876543210fedcba9876543210",
		"reload_request_id": "0123456789abcdef0123456789abcdef",
	})
	info := EnvelopeOf(err).Error
	if info.Code != CodeRollbackIncomplete || len(info.Causes) != 2 {
		t.Fatalf("rollback envelope = %+v", info)
	}
	if info.Causes[0].Code != CodeReloadRejected || info.Causes[1].Code != CodeReloadTimeout {
		t.Fatalf("rollback causes = %+v", info.Causes)
	}
}

func TestDetailsAreAllowlistedAndScalar(t *testing.T) {
	now := time.Now().UTC()
	err := New(CodeServiceOperationBusy, "service.start", "busy").WithDetails(map[string]any{
		"holder_pid":        42,
		"started_at":        now,
		"operation_id":      "safe",
		"config":            "secret",
		"token":             "secret",
		"reload_request_id": []string{"not", "scalar"},
	})
	want := map[string]any{"holder_pid": 42, "started_at": now, "operation_id": "safe"}
	if !reflect.DeepEqual(err.Details, want) {
		t.Fatalf("details = %#v, want %#v", err.Details, want)
	}
}

func TestInfoRoundTrip(t *testing.T) {
	original := Wrap(CodeRollbackIncomplete, "service.reload", "rollback incomplete", errors.Join(
		New(CodeReloadRejected, "service.reload", "apply rejected"),
		New(CodeReloadTimeout, "service.reload", "rollback timeout"),
	)).WithDetails(map[string]any{"phase": "rollback"})
	want := InfoOf(original)
	restored := FromInfo(want)
	got := InfoOf(restored)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
	var typed *Error
	if !errors.As(restored, &typed) || typed.Code != CodeRollbackIncomplete {
		t.Fatalf("restored error = %#v", restored)
	}
}

func TestValidateInfoRejectsInvalidContract(t *testing.T) {
	valid := ErrorInfo{Code: CodeReloadRejected, Message: "rejected", Retryable: false, Causes: []ErrorInfo{}}
	tests := []struct {
		name string
		info ErrorInfo
	}{
		{name: "unknown code", info: ErrorInfo{Code: "NOPE", Message: "bad", Causes: []ErrorInfo{}}},
		{name: "missing message", info: ErrorInfo{Code: CodeReloadRejected, Causes: []ErrorInfo{}}},
		{name: "retryable mismatch", info: ErrorInfo{Code: CodeReloadTimeout, Message: "timeout", Retryable: false, Causes: []ErrorInfo{}}},
		{name: "unsafe details", info: ErrorInfo{Code: CodeReloadRejected, Message: "bad", Details: map[string]any{"token": "secret"}, Causes: []ErrorInfo{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateInfo(test.info); err == nil {
				t.Fatalf("ValidateInfo(%+v) succeeded", test.info)
			}
			if got := CodeOf(FromInfo(test.info)); got != CodeServiceProtocolTooOld {
				t.Fatalf("FromInfo code = %s", got)
			}
		})
	}
	if err := ValidateInfo(valid); err != nil {
		t.Fatalf("valid info rejected: %v", err)
	}
}

func TestEnvelopeJSONSchema(t *testing.T) {
	encoded, err := json.Marshal(EnvelopeOf(New(CodeUsageError, "cli", "invalid usage")))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if ok, _ := document["ok"].(bool); ok {
		t.Fatalf("ok = %v", document["ok"])
	}
	errorObject, ok := document["error"].(map[string]any)
	if !ok || errorObject["code"] != string(CodeUsageError) || errorObject["message"] != "invalid usage" {
		t.Fatalf("error document = %#v", document)
	}
	if causes, ok := errorObject["causes"].([]any); !ok || len(causes) != 0 {
		t.Fatalf("causes = %#v", errorObject["causes"])
	}
}

func TestInfoOfLimitsTotalCauseCountForRoundTrip(t *testing.T) {
	causes := make([]error, 0, maxCauseCount+8)
	for range maxCauseCount + 8 {
		causes = append(causes, New(CodeReloadRejected, "service.reload", "rejected"))
	}
	info := InfoOf(errors.Join(causes...))
	if got, want := len(info.Causes), maxCauseCount-1; got != want {
		t.Fatalf("cause count = %d, want %d", got, want)
	}
	if err := ValidateInfo(info); err != nil {
		t.Fatalf("InfoOf produced invalid contract: %v", err)
	}
	if got := InfoOf(FromInfo(info)); !reflect.DeepEqual(got, info) {
		t.Fatalf("round trip = %#v, want %#v", got, info)
	}
}
