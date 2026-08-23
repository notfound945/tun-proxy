package apperror

import (
	"errors"
	"fmt"
)

const (
	maxCauseDepth = 8
	maxCauseCount = 32
)

// Envelope is emitted by the CLI in JSON output mode.
type Envelope struct {
	OK    bool      `json:"ok"`
	Error ErrorInfo `json:"error"`
}

// ErrorInfo is the transport-safe representation used by both the CLI JSON
// contract and internal control protocols.
type ErrorInfo struct {
	Code      Code           `json:"code"`
	Operation string         `json:"operation,omitempty"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
	Causes    []ErrorInfo    `json:"causes"`
}

func EnvelopeOf(err error) Envelope {
	return Envelope{OK: false, Error: InfoOf(err)}
}

func InfoOf(err error) ErrorInfo {
	remaining := maxCauseCount
	return infoOf(err, 0, &remaining)
}

func infoOf(err error, depth int, remaining *int) ErrorInfo {
	*remaining--
	if err == nil {
		return ErrorInfo{Code: CodeInternalError, Message: "unknown error", Causes: []ErrorInfo{}}
	}
	if depth >= maxCauseDepth {
		return ErrorInfo{Code: CodeInternalError, Message: "error cause depth exceeded", Causes: []ErrorInfo{}}
	}

	// Preserve the shape of a direct errors.Join before errors.As selects only
	// the first typed child. Typed errors with a joined Cause are handled by
	// the branch below and retain their own stable outer code.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return ErrorInfo{
			Code: CodeInternalError, Message: "multiple errors occurred", Retryable: false,
			Causes: causeInfos(joined.Unwrap(), depth+1, remaining),
		}
	}

	var typed *Error
	if errors.As(err, &typed) && typed != nil {
		info := ErrorInfo{
			Code: normalizedCode(typed.Code), Operation: typed.Operation, Message: typed.Message,
			Retryable: typed.Retryable, Details: sanitizeDetails(typed.Details), Causes: []ErrorInfo{},
		}
		if info.Message == "" {
			info.Message = string(info.Code)
		}
		if joined, ok := typed.Cause.(interface{ Unwrap() []error }); ok {
			info.Causes = causeInfos(joined.Unwrap(), depth+1, remaining)
		}
		return info
	}

	return ErrorInfo{Code: CodeInternalError, Message: err.Error(), Causes: []ErrorInfo{}}
}

func causeInfos(causes []error, depth int, remaining *int) []ErrorInfo {
	capacity := len(causes)
	if capacity > *remaining {
		capacity = *remaining
	}
	result := make([]ErrorInfo, 0, capacity)
	for _, cause := range causes {
		if *remaining == 0 {
			break
		}
		if cause != nil {
			result = append(result, infoOf(cause, depth, remaining))
		}
	}
	return result
}

// FromInfo reconstructs an Error while retaining nested causes for
// errors.Is/errors.As traversal inside the receiving process.
func FromInfo(info ErrorInfo) error {
	if err := ValidateInfo(info); err != nil {
		return Wrap(CodeServiceProtocolTooOld, "service.protocol", "received an invalid structured service error", err)
	}
	causes := make([]error, 0, len(info.Causes))
	for _, cause := range info.Causes {
		causes = append(causes, FromInfo(cause))
	}
	var cause error
	if len(causes) > 0 {
		cause = errors.Join(causes...)
	}
	return (&Error{
		Code: info.Code, Operation: info.Operation, Message: info.Message,
		Retryable: info.Retryable, Details: sanitizeDetails(info.Details), Cause: cause,
	})
}

func ValidateInfo(info ErrorInfo) error {
	return validateInfo(info, 0, new(int))
}

func validateInfo(info ErrorInfo, depth int, count *int) error {
	(*count)++
	if *count > maxCauseCount || depth > maxCauseDepth {
		return errors.New("structured error has too many causes")
	}
	if !IsKnownCode(info.Code) {
		return fmt.Errorf("structured error code %q is unknown", info.Code)
	}
	if info.Message == "" {
		return errors.New("structured error message is required")
	}
	if info.Retryable != Retryable(info.Code) {
		return fmt.Errorf("structured error retryable=%t does not match code %s", info.Retryable, info.Code)
	}
	if len(info.Details) != len(sanitizeDetails(info.Details)) {
		return errors.New("structured error details contain unsupported fields or values")
	}
	for _, cause := range info.Causes {
		if err := validateInfo(cause, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}
