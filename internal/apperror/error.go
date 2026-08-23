package apperror

import (
	"errors"
	"fmt"
	"time"
)

// Error is the typed application error carried across package and process
// boundaries. Message is a stable user-facing summary; Cause retains the full
// Go error chain for errors.Is/errors.As and text diagnostics.
type Error struct {
	Code      Code
	Operation string
	Message   string
	Retryable bool
	Details   map[string]any
	Cause     error
}

func New(code Code, operation, message string) *Error {
	return &Error{Code: normalizedCode(code), Operation: operation, Message: message, Retryable: Retryable(normalizedCode(code))}
}

func Wrap(code Code, operation, message string, cause error) *Error {
	err := New(code, operation, message)
	err.Cause = cause
	return err
}

func (err *Error) Error() string {
	if err == nil {
		return ""
	}
	message := err.Message
	if message == "" {
		message = string(normalizedCode(err.Code))
	}
	if err.Cause == nil {
		return message
	}
	return fmt.Sprintf("%s: %v", message, err.Cause)
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

// WithDetails returns a copy with only explicitly permitted, scalar diagnostic
// fields. Unknown keys and unsafe values are discarded rather than serialized.
func (err *Error) WithDetails(details map[string]any) *Error {
	if err == nil {
		return nil
	}
	clone := *err
	clone.Details = sanitizeDetails(details)
	return &clone
}

// Annotate returns a typed copy with additional safe diagnostic context. It
// preserves the existing stable code/message and underlying cause chain.
func Annotate(err error, operation string, details map[string]any) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) && typed != nil {
		clone := *typed
		if operation != "" {
			clone.Operation = operation
		}
		merged := make(map[string]any, len(typed.Details)+len(details))
		for key, value := range typed.Details {
			merged[key] = value
		}
		for key, value := range details {
			merged[key] = value
		}
		clone.Details = sanitizeDetails(merged)
		return &clone
	}
	return Wrap(CodeInternalError, operation, "operation failed", err).WithDetails(details)
}

func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	if _, joined := err.(interface{ Unwrap() []error }); joined {
		return CodeInternalError
	}
	var typed *Error
	if errors.As(err, &typed) && typed != nil {
		return normalizedCode(typed.Code)
	}
	return CodeInternalError
}

func ExitCodeOf(err error) int {
	return ExitCode(CodeOf(err))
}

func normalizedCode(code Code) Code {
	if !IsKnownCode(code) {
		return CodeInternalError
	}
	return code
}

var allowedDetailKeys = map[string]struct{}{
	"actual_config_digest":   {},
	"effective_uid":          {},
	"expected_config_digest": {},
	"holder_operation":       {},
	"holder_operation_id":    {},
	"holder_pid":             {},
	"operation_id":           {},
	"phase":                  {},
	"reload_request_id":      {},
	"rollback_of":            {},
	"socket_path":            {},
	"started_at":             {},
}

func sanitizeDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	clean := make(map[string]any)
	for key, value := range details {
		if _, ok := allowedDetailKeys[key]; !ok || !safeDetailValue(value) {
			continue
		}
		clean[key] = value
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

func safeDetailValue(value any) bool {
	switch value.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, time.Time:
		return true
	default:
		return false
	}
}
