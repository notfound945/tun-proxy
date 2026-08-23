// Package control exposes the root supervisor's authenticated management
// protocol. Unlike the worker-owned status socket, every successful response
// represents a completed supervisor/worker operation.
package control

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	Version        = 1
	KindReload     = "reload"
	maxMessageSize = 8 << 10
	maxErrorSize   = 4 << 10
)

// ReloadRequest asks the supervisor to load the managed configuration whose
// exact bytes hash to ExpectedConfigDigest.
type ReloadRequest struct {
	Version              int    `json:"version"`
	Kind                 string `json:"kind"`
	ExpectedConfigDigest string `json:"expected_config_digest"`
}

// ReloadResponse is sent only after the worker has accepted or rejected the
// reload and the supervisor has persisted its resulting state.
type ReloadResponse struct {
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	ConfigDigest string `json:"config_digest,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (request ReloadRequest) validate() error {
	var failures []error
	if request.Version != Version {
		failures = append(failures, fmt.Errorf("control request version must be %d, got %d", Version, request.Version))
	}
	if request.Kind != KindReload {
		failures = append(failures, fmt.Errorf("control request kind must be %q, got %q", KindReload, request.Kind))
	}
	if err := validateDigest(request.ExpectedConfigDigest); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (response ReloadResponse) validate() error {
	var failures []error
	if response.Version != Version {
		failures = append(failures, fmt.Errorf("control response version must be %d, got %d", Version, response.Version))
	}
	if response.Kind != KindReload {
		failures = append(failures, fmt.Errorf("control response kind must be %q, got %q", KindReload, response.Kind))
	}
	if len(response.Error) > maxErrorSize {
		failures = append(failures, fmt.Errorf("control response error exceeds %d bytes", maxErrorSize))
	}
	if response.Error == "" {
		if err := validateDigest(response.ConfigDigest); err != nil {
			failures = append(failures, err)
		}
	} else if response.ConfigDigest != "" {
		failures = append(failures, errors.New("failed control response must not include a config digest"))
	}
	return errors.Join(failures...)
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
