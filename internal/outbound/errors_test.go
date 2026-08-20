package outbound

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestIsRecoverable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"interface unavailable", &InterfaceUnavailableError{Interface: "en5", Cause: errors.New("gone")}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"no route", syscall.ENETUNREACH, true},
		{"connection refused", syscall.ECONNREFUSED, false},
		{"policy reject", ErrRejected, false},
		{"caller cancellation", context.Canceled, false},
		{"all joined failures recoverable", errors.Join(syscall.ENETUNREACH, context.DeadlineExceeded), true},
		{"mixed joined failures terminal", errors.Join(syscall.ENETUNREACH, syscall.ECONNREFUSED), false},
		{"wrapped mixed failures terminal", fmt.Errorf("all failed: %w", errors.Join(context.DeadlineExceeded, syscall.ECONNREFUSED)), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRecoverable(test.err); got != test.want {
				t.Fatalf("IsRecoverable(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}
