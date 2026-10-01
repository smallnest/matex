// Package errs defines the error model of matex.
//
// Every error carries a Kind (mapped to an HTTP status), a business Code
// and a human readable message. The HTTP layer renders them uniformly as
// {"code":<code>,"msg":"<msg>"} with the matching status code.
//
// Recommended code scheme: <http-status>*100 + sub, e.g. 40401 for a
// "user not found" NotFound error. Codes are per-service constants.
package errs

import (
	"errors"
	"fmt"
)

// Kind classifies an error and determines its HTTP status.
type Kind int

const (
	KindInvalidArgument Kind = iota + 1 // 400
	KindUnauthorized                    // 401
	KindForbidden                       // 403
	KindNotFound                        // 404
	KindConflict                        // 409
	KindRateLimited                     // 429
	KindInternal                        // 500
	KindUnavailable                     // 503
	KindTimeout                         // 504
)

// HTTPStatus maps the kind to its HTTP status code.
func (k Kind) HTTPStatus() int {
	switch k {
	case KindInvalidArgument:
		return 400
	case KindUnauthorized:
		return 401
	case KindForbidden:
		return 403
	case KindNotFound:
		return 404
	case KindConflict:
		return 409
	case KindRateLimited:
		return 429
	case KindInternal:
		return 500
	case KindUnavailable:
		return 503
	case KindTimeout:
		return 504
	}
	return 500
}

// Error is the matex error type.
type Error struct {
	Kind  Kind
	Code  int
	Msg   string
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Msg + ": " + e.Cause.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Cause }

// New creates an Error.
func New(kind Kind, code int, msg string) *Error {
	return &Error{Kind: kind, Code: code, Msg: msg}
}

// Newf creates an Error with a formatted message.
func Newf(kind Kind, code int, format string, a ...any) *Error {
	return &Error{Kind: kind, Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Wrap creates an Error wrapping cause.
func Wrap(kind Kind, code int, cause error, msg string) *Error {
	return &Error{Kind: kind, Code: code, Msg: msg, Cause: cause}
}

// Status extracts (httpStatus, code, msg) from err. Non-matex errors are
// reported as internal errors without leaking details.
func Status(err error) (httpStatus, code int, msg string) {
	if e, ok := errors.AsType[*Error](err); ok {
		c := e.Code
		if c == 0 {
			c = e.Kind.HTTPStatus() * 100
		}
		return e.Kind.HTTPStatus(), c, e.Msg
	}
	return 500, 50000, "internal error"
}

// KindOf returns the Kind of err (KindInternal for non-matex errors).
func KindOf(err error) Kind {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	return KindInternal
}

// Shorthand constructors. Code 0 falls back to http-status*100.

func Invalid(code int, format string, a ...any) *Error {
	return Newf(KindInvalidArgument, code, format, a...)
}

func Unauthorized(code int, format string, a ...any) *Error {
	return Newf(KindUnauthorized, code, format, a...)
}

func Forbidden(code int, format string, a ...any) *Error {
	return Newf(KindForbidden, code, format, a...)
}

func NotFound(code int, format string, a ...any) *Error {
	return Newf(KindNotFound, code, format, a...)
}

func Conflict(code int, format string, a ...any) *Error {
	return Newf(KindConflict, code, format, a...)
}

func RateLimited(code int, format string, a ...any) *Error {
	return Newf(KindRateLimited, code, format, a...)
}

func Internal(code int, format string, a ...any) *Error {
	return Newf(KindInternal, code, format, a...)
}

func InternalWrap(code int, cause error, format string, a ...any) *Error {
	return Wrap(KindInternal, code, cause, fmt.Sprintf(format, a...))
}

func Unavailable(code int, format string, a ...any) *Error {
	return Newf(KindUnavailable, code, format, a...)
}

func Timeout(code int, format string, a ...any) *Error {
	return Newf(KindTimeout, code, format, a...)
}
