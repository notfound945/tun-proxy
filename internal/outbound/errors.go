package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

var ErrRejected = errors.New("connection rejected by outbound policy")

// InterfaceUnavailableError marks failures that may safely advance through a
// configured fallback chain.
type InterfaceUnavailableError struct {
	Interface string
	Cause     error
}

func (err *InterfaceUnavailableError) Error() string {
	return fmt.Sprintf("interface %q is unavailable: %v", err.Interface, err.Cause)
}

func (err *InterfaceUnavailableError) Unwrap() error { return err.Cause }

// IsRecoverable reports only transport/environment failures for which trying
// a different outbound is meaningful. Application-level refusal is terminal.
func IsRecoverable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrRejected) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsRecoverable(child) {
				return false
			}
		}
		return true
	}
	if _, ok := err.(*InterfaceUnavailableError); ok {
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		return IsRecoverable(unwrapped)
	}
	errno, ok := err.(syscall.Errno)
	return ok && (errno == syscall.ENETDOWN ||
		errno == syscall.ENETUNREACH ||
		errno == syscall.EHOSTUNREACH ||
		errno == syscall.ETIMEDOUT ||
		errno == syscall.ENXIO ||
		errno == syscall.EADDRNOTAVAIL)
}
