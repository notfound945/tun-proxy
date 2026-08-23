// Package apperror defines the stable, machine-readable error contract shared
// by the CLI and the managed-service control protocols.
package apperror

type Code string

const (
	CodeUsageError            Code = "USAGE_ERROR"
	CodeRootRequired          Code = "ROOT_REQUIRED"
	CodeConfigInvalid         Code = "CONFIG_INVALID"
	CodeConfigRestartRequired Code = "CONFIG_RESTART_REQUIRED"
	CodeServiceNotInstalled   Code = "SERVICE_NOT_INSTALLED"
	CodeServiceNotRunning     Code = "SERVICE_NOT_RUNNING"
	CodeServiceOperationBusy  Code = "SERVICE_OPERATION_BUSY"
	CodeServiceStartTimeout   Code = "SERVICE_START_TIMEOUT"
	CodeServiceStopTimeout    Code = "SERVICE_STOP_TIMEOUT"
	CodeServiceUnreachable    Code = "SERVICE_UNREACHABLE"
	CodeServiceProtocolTooOld Code = "SERVICE_PROTOCOL_TOO_OLD"
	CodeReloadRejected        Code = "RELOAD_REJECTED"
	CodeReloadTimeout         Code = "RELOAD_TIMEOUT"
	CodeReloadRequestMismatch Code = "RELOAD_REQUEST_MISMATCH"
	CodeReloadDigestMismatch  Code = "RELOAD_DIGEST_MISMATCH"
	CodeReloadRequestConflict Code = "RELOAD_REQUEST_CONFLICT"
	CodeRollbackIncomplete    Code = "ROLLBACK_INCOMPLETE"
	CodeUnsafeFile            Code = "UNSAFE_FILE"
	CodeInternalError         Code = "INTERNAL_ERROR"
)

var knownCodes = map[Code]struct{}{
	CodeUsageError: {}, CodeRootRequired: {}, CodeConfigInvalid: {}, CodeConfigRestartRequired: {},
	CodeServiceNotInstalled: {}, CodeServiceNotRunning: {}, CodeServiceOperationBusy: {},
	CodeServiceStartTimeout: {}, CodeServiceStopTimeout: {}, CodeServiceUnreachable: {},
	CodeServiceProtocolTooOld: {}, CodeReloadRejected: {}, CodeReloadTimeout: {},
	CodeReloadRequestMismatch: {}, CodeReloadDigestMismatch: {}, CodeReloadRequestConflict: {},
	CodeRollbackIncomplete: {}, CodeUnsafeFile: {}, CodeInternalError: {},
}

func IsKnownCode(code Code) bool {
	_, ok := knownCodes[code]
	return ok
}

func Retryable(code Code) bool {
	switch code {
	case CodeServiceNotRunning, CodeServiceOperationBusy, CodeServiceStartTimeout,
		CodeServiceStopTimeout, CodeServiceUnreachable, CodeReloadTimeout:
		return true
	default:
		return false
	}
}

// ExitCode maps exact string codes to intentionally coarse process exit
// classes. Callers should use Code for program logic and ExitCode only for
// shell-level routing.
func ExitCode(code Code) int {
	switch code {
	case CodeUsageError:
		return 2
	case CodeConfigInvalid, CodeConfigRestartRequired:
		return 3
	case CodeRootRequired, CodeUnsafeFile:
		return 4
	case CodeServiceNotInstalled, CodeServiceNotRunning, CodeReloadRejected,
		CodeReloadRequestMismatch, CodeReloadDigestMismatch:
		return 5
	case CodeServiceStartTimeout, CodeServiceStopTimeout, CodeServiceUnreachable, CodeReloadTimeout:
		return 6
	case CodeRollbackIncomplete:
		return 7
	case CodeServiceOperationBusy, CodeReloadRequestConflict:
		return 8
	case CodeServiceProtocolTooOld:
		return 9
	default:
		return 1
	}
}
