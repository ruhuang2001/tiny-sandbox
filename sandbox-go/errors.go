package sandbox

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors. errors.Is matches these on every error the SDK returns for
// the corresponding outcome, including errors that also carry an Execution.
var (
	ErrNonZeroExit       = errors.New("sandbox: command exited with a nonzero code")
	ErrTimeout           = errors.New("sandbox: sandbox lifetime expired")
	ErrCanceled          = errors.New("sandbox: execution canceled")
	ErrOOM               = errors.New("sandbox: killed by the kernel OOM killer")
	ErrEvicted           = errors.New("sandbox: container was stopped externally")
	ErrCancelUnconfirmed = errors.New("sandbox: could not confirm the server stopped the work")
	ErrNotFound          = errors.New("sandbox: not found")
	ErrUnauthorized      = errors.New("sandbox: unauthorized")
	ErrBusy              = errors.New("sandbox: another execution is running")
	ErrInvalidState      = errors.New("sandbox: operation invalid in the current state")
)

// Error is every error the SDK returns. It carries the HTTP status and API
// code for server errors, the full Execution for execution outcomes, and the
// idempotency key when a recovery could not be confirmed.
type Error struct {
	StatusCode     int    // 0 for client-side errors
	Code           string // API code, or canceled / cancel_unconfirmed / nonzero_exit / a reason name
	Message        string
	Execution      *Execution // set for execution outcomes; carries full output and exit code
	IdempotencyKey string     // set for cancel_unconfirmed during start
	Cause          error
}

// Error renders the message with its cause.
func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "sandbox: request failed"
	}
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is matches sentinel values by the outcome and status this error represents.
// An execution outcome matches both its termination reason and, for a nonzero
// exit, ErrNonZeroExit.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound || e.Code == "not_found"
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized || e.Code == "unauthorized"
	case ErrBusy:
		return e.Code == "busy"
	case ErrInvalidState:
		return e.Code == "invalid_state"
	case ErrCancelUnconfirmed:
		return e.Code == "cancel_unconfirmed"
	case ErrCanceled:
		return e.Code == string(ReasonCanceled) || e.Code == "canceled"
	case ErrTimeout:
		return e.Code == string(ReasonTimeout)
	case ErrOOM:
		return e.Code == string(ReasonOOM)
	case ErrEvicted:
		return e.Code == string(ReasonEvicted)
	case ErrNonZeroExit:
		return e.Code == "nonzero_exit" ||
			(e.Execution != nil && e.Execution.ExitCode != nil && *e.Execution.ExitCode != 0)
	default:
		return false
	}
}

// Retryable reports whether the error is safe to retry from the caller's point
// of view. The transport retries inside the SDK already used this rule, so a
// caller sees true only when the whole retry budget was exhausted.
func (e *Error) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.StatusCode == 0 && e.Code == "transport"
}

// outcomeError turns a completed execution into an error, or nil when it
// exited 0. The Execution is always attached so no output is lost.
func outcomeError(ex *Execution) error {
	if ex == nil {
		return nil
	}
	if ex.TerminationReason == nil {
		return &Error{Code: "incomplete", Message: "sandbox: execution did not complete", Execution: ex}
	}
	switch *ex.TerminationReason {
	case ReasonCanceled:
		return &Error{Code: string(ReasonCanceled), Message: "sandbox: execution canceled", Execution: ex}
	case ReasonTimeout:
		return &Error{Code: string(ReasonTimeout), Message: "sandbox: sandbox lifetime expired", Execution: ex}
	case ReasonOOM:
		return &Error{Code: string(ReasonOOM), Message: "sandbox: killed by the kernel OOM killer", Execution: ex}
	case ReasonEvicted:
		return &Error{Code: string(ReasonEvicted), Message: "sandbox: container was stopped externally", Execution: ex}
	}
	if ex.ExitCode == nil {
		return &Error{Code: "nonzero_exit", Message: "sandbox: the runtime reported no exit code", Execution: ex}
	}
	if *ex.ExitCode != 0 {
		return &Error{
			Code:      "nonzero_exit",
			Message:   fmt.Sprintf("sandbox: command exited %d", *ex.ExitCode),
			Execution: ex,
		}
	}
	return nil
}
