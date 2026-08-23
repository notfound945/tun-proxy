// Package control exposes the root supervisor's authenticated management
// protocol. Unlike the worker-owned status socket, every successful response
// represents a completed supervisor/worker operation.
package control

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	Version          = 2
	KindReload       = "reload"
	KindReloadResult = "reload_result"
	ResultRunning    = "running"
	ResultSucceeded  = "succeeded"
	ResultFailed     = "failed"
	maxMessageSize   = 8 << 10
	maxErrorSize     = 4 << 10
	requestIDBytes   = 16
)

const unknownRequestID = "00000000000000000000000000000000"

// ReloadRequest asks the supervisor to load the managed configuration whose
// exact bytes hash to ExpectedConfigDigest. RequestID is stable across retries;
// OperationID identifies the complete CLI transaction that owns this reload.
type ReloadRequest struct {
	Version              int    `json:"version"`
	Kind                 string `json:"kind"`
	RequestID            string `json:"request_id"`
	OperationID          string `json:"operation_id"`
	RollbackOf           string `json:"rollback_of,omitempty"`
	ExpectedConfigDigest string `json:"expected_config_digest"`
}

// ReloadResponse describes the current or final state of one idempotent reload
// request. Final responses are cached by the supervisor control server so a
// client can recover a response lost after the reload was already applied.
type ReloadResponse struct {
	Version      int       `json:"version"`
	Kind         string    `json:"kind"`
	RequestID    string    `json:"request_id"`
	Result       string    `json:"result"`
	ConfigDigest string    `json:"config_digest,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// NewRequestID returns a cryptographically random 128-bit lowercase hex ID.
func NewRequestID() (string, error) {
	contents := make([]byte, requestIDBytes)
	if _, err := rand.Read(contents); err != nil {
		return "", fmt.Errorf("generate reload request ID: %w", err)
	}
	return hex.EncodeToString(contents), nil
}

func (request ReloadRequest) validate() error {
	var failures []error
	if request.Version != Version {
		failures = append(failures, fmt.Errorf("control request version must be %d, got %d", Version, request.Version))
	}
	if request.Kind != KindReload {
		failures = append(failures, fmt.Errorf("control request kind must be %q, got %q", KindReload, request.Kind))
	}
	if err := validateID("reload request ID", request.RequestID); err != nil {
		failures = append(failures, err)
	}
	if err := validateID("operation ID", request.OperationID); err != nil {
		failures = append(failures, err)
	}
	if request.RollbackOf != "" {
		if err := validateID("rollback request ID", request.RollbackOf); err != nil {
			failures = append(failures, err)
		} else if request.RollbackOf == request.RequestID {
			failures = append(failures, errors.New("rollback_of must differ from request_id"))
		}
	}
	if err := validateDigest(request.ExpectedConfigDigest); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (request ReloadRequest) sameIdentity(other ReloadRequest) bool {
	return request.ExpectedConfigDigest == other.ExpectedConfigDigest &&
		request.OperationID == other.OperationID && request.RollbackOf == other.RollbackOf
}

func (response ReloadResponse) validate() error {
	var failures []error
	if response.Version != Version {
		failures = append(failures, fmt.Errorf("control response version must be %d, got %d", Version, response.Version))
	}
	if response.Kind != KindReloadResult {
		failures = append(failures, fmt.Errorf("control response kind must be %q, got %q", KindReloadResult, response.Kind))
	}
	if err := validateID("reload response request ID", response.RequestID); err != nil {
		failures = append(failures, err)
	}
	if response.StartedAt.IsZero() {
		failures = append(failures, errors.New("control response started_at is required"))
	}
	if len(response.Error) > maxErrorSize {
		failures = append(failures, fmt.Errorf("control response error exceeds %d bytes", maxErrorSize))
	}
	switch response.Result {
	case ResultRunning:
		if !response.CompletedAt.IsZero() || response.ConfigDigest != "" || response.Error != "" {
			failures = append(failures, errors.New("running control response must not include completion, digest, or error"))
		}
	case ResultSucceeded:
		if response.CompletedAt.IsZero() {
			failures = append(failures, errors.New("successful control response completed_at is required"))
		}
		if err := validateDigest(response.ConfigDigest); err != nil {
			failures = append(failures, err)
		}
		if response.Error != "" {
			failures = append(failures, errors.New("successful control response must not include an error"))
		}
	case ResultFailed:
		if response.CompletedAt.IsZero() {
			failures = append(failures, errors.New("failed control response completed_at is required"))
		}
		if response.Error == "" {
			failures = append(failures, errors.New("failed control response error is required"))
		}
		if response.ConfigDigest != "" {
			failures = append(failures, errors.New("failed control response must not include a config digest"))
		}
	default:
		failures = append(failures, fmt.Errorf("control response result is invalid: %q", response.Result))
	}
	if !response.CompletedAt.IsZero() && !response.StartedAt.IsZero() && response.CompletedAt.Before(response.StartedAt) {
		failures = append(failures, errors.New("control response completed_at precedes started_at"))
	}
	return errors.Join(failures...)
}

func validateID(name, value string) error {
	if len(value) != requestIDBytes*2 {
		return fmt.Errorf("%s must be 32 lowercase hex characters, got %q", name, value)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("%s must use lowercase hexadecimal, got %q", name, value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s is invalid: %w", name, err)
	}
	return nil
}

func validateDigest(digest string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+64 {
		return fmt.Errorf("config digest must be sha256:<64 hex characters>, got %q", digest)
	}
	encoded := strings.TrimPrefix(digest, prefix)
	if encoded != strings.ToLower(encoded) {
		return fmt.Errorf("config digest must use lowercase hexadecimal, got %q", digest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("config digest is invalid: %w", err)
	}
	return nil
}
