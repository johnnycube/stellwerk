package ports

import (
	"errors"
	"time"
)

// Senders classify their errors with these wrappers, so the core can retry
// sensibly: a permanent error (bad address, 403, 550 no such user) is not
// retried, and a backend that says when to come back is not hammered.
// Unwrapped errors count as transient.

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying. nil stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err, or an error it wraps, is permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type retryAfterError struct {
	err   error
	after time.Duration
}

func (e retryAfterError) Error() string { return e.err.Error() }
func (e retryAfterError) Unwrap() error { return e.err }

// RetryAfter says the backend asked to wait d before the next attempt
// (HTTP 429 Retry-After, Matrix M_LIMIT_EXCEEDED). nil stays nil.
func RetryAfter(err error, d time.Duration) error {
	if err == nil {
		return nil
	}
	return retryAfterError{err, d}
}

// RetryAfterOf returns the wait a backend asked for, if any.
func RetryAfterOf(err error) (time.Duration, bool) {
	var r retryAfterError
	if errors.As(err, &r) && r.after > 0 {
		return r.after, true
	}
	return 0, false
}
