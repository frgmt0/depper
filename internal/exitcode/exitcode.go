// Package exitcode carries process exit codes through ordinary Go errors.
package exitcode

import "errors"

// Process exit codes used by every depper command.
const (
	OK        = 0 // success
	Failure   = 1 // any error
	Unhealthy = 2 // health check failed
	Drift     = 3 // deployment outdated or secrets drift
)

// Error attaches an exit code to an error. A nil Err makes the error silent:
// the process exits with Code without printing a message (used when a command
// has already reported its result, e.g. an unhealthy health check).
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// New wraps err with an exit code.
func New(code int, err error) error { return &Error{Code: code, Err: err} }

// Silent returns an error that only sets the exit code.
func Silent(code int) error { return &Error{Code: code} }

// Code maps an error to a process exit code: nil is OK, an *Error carries its
// own code, and anything else is Failure.
func Code(err error) int {
	if err == nil {
		return OK
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return Failure
}
