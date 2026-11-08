package daemon

import (
	"errors"
	"fmt"
)

// Code is one of the error codes from the HTTP contract in docs/api.md.
type Code string

const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeUnauthorized        Code = "unauthorized"
	CodeNotFound            Code = "not_found"
	CodeInvalidState        Code = "invalid_state"
	CodeBusy                Code = "busy"
	CodeIdempotencyConflict Code = "idempotency_conflict"
	CodePayloadTooLarge     Code = "payload_too_large"
	CodeRateLimited         Code = "rate_limited"
	CodeInternal            Code = "internal"
	CodeBackendError        Code = "backend_error"
	CodeUnavailable         Code = "unavailable"
)

// Error is a contract error: a stable code plus the message that is sent to
// clients. Err is the underlying cause and is never serialized.
type Error struct {
	Code    Code
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the contract code of err. Errors that are not *Error are
// internal failures.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// ErrNotFound is the sentinel a Backend returns when a sandbox-side resource,
// such as a file, does not exist. The daemon maps it to not_found.
var ErrNotFound = errors.New("daemon: not found")

// backendError wraps a container-runtime failure. A backend that reports
// ErrNotFound keeps the not_found code so file reads do not become 502s.
func backendError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return &Error{Code: CodeNotFound, Message: err.Error(), Err: err}
	}
	return &Error{Code: CodeBackendError, Message: "container runtime failed", Err: err}
}
