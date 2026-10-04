// Package service holds every use case exactly once.
//
// internal/api and internal/mcp are adapters over it: they parse transport
// input, call one method, and render what comes back. Neither reaches past it
// to the store, the sync engine or a provider — a rule the linter enforces —
// because the moment either does, REST and MCP start to disagree about what
// the product does, which is the single thing this design promises not to let
// happen.
//
// This file is the error vocabulary. It is closed on purpose: seven codes that
// every transport knows how to render, so a caller can branch on them without
// reading prose, and a new failure mode has to be argued into one of the
// existing meanings rather than quietly inventing an eighth.
package service

import (
	"errors"
	"fmt"
	"time"
)

// Code is the machine-readable half of an error.
type Code string

const (
	// CodeUnauthorized is a missing, malformed, revoked or expired key.
	CodeUnauthorized Code = "unauthorized"
	// CodeNotAuthorized is a valid key that may not do this: the scope is too
	// low, or the key is restricted to other accounts.
	CodeNotAuthorized Code = "not_authorized"
	// CodeBadRequest is input the caller can fix: a malformed address, an
	// unknown parameter, a send without confirmation.
	CodeBadRequest Code = "bad_request"
	// CodeNotFound is a message, folder, account or draft that does not exist
	// — or that this caller may not know exists.
	CodeNotFound Code = "not_found"
	// CodeConflict is a request that collides with the current state: an
	// account waiting for re-authorisation, a draft already sent, an
	// idempotency key reused with a different body, a send whose outcome is
	// unknown.
	CodeConflict Code = "conflict"
	// CodeRateLimited is our own limiter or the provider's throttling.
	CodeRateLimited Code = "rate_limited"
	// CodeInternal is everything else, including an upstream that is simply
	// down. The message says "upstream:" when the fault is not ours.
	CodeInternal Code = "internal"
)

// Error is what every transport renders. The message is safe to show a caller:
// it never carries a credential, and never repeats a server's raw response at
// length.
type Error struct {
	Code    Code
	Message string
	// Retry, when set, becomes a Retry-After header and tells a model how long
	// to wait rather than to try again immediately.
	Retry time.Duration
	// Err is the cause, for logs only. It is never serialised.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.Err.Error()
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is the same error as e apart from its cause.
//
// The common errors below are returned with the failure that led to them
// attached, so a log can say why an account needs re-authorisation rather
// than only that it does. Attaching a cause makes a new value, and without
// this errors.Is(err, ErrNeedsReauth) would stop recognising it. Only a
// target with no cause of its own matches, which is what the common errors
// are.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Err == nil && t.Code == e.Code && t.Message == e.Message && t.Retry == e.Retry
}

// E builds an error.
func E(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Err: cause}
}

// Ef builds an error with a formatted message. The format string is a constant
// at the call site; values interpolated into it must already be safe to show.
func Ef(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: cause}
}

// Retryable builds a rate-limited error carrying how long to wait.
func Retryable(message string, after time.Duration, cause error) *Error {
	return &Error{Code: CodeRateLimited, Message: message, Retry: after, Err: cause}
}

// CodeOf classifies an error. Anything unrecognised is internal, so a bug
// cannot accidentally present itself as a client mistake.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// MessageOf is the caller-facing message for err.
func MessageOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	// Deliberately not err.Error(): an unclassified failure may quote a
	// database error, a server banner or a file path, none of which a caller
	// should see.
	return "internal error"
}

// RetryAfterOf is how long to wait before trying again, or zero.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.Retry
	}
	return 0
}

// Common errors that several call sites produce identically.
var (
	// ErrConfirmRequired guards every send. The message doubles as the
	// instruction an LLM needs to recover.
	ErrConfirmRequired = &Error{
		Code:    CodeBadRequest,
		Message: "sending requires confirm=true; call create_draft and send_draft to review the message first",
	}
	// ErrNeedsReauth is an account whose grant is dead: its refresh token,
	// or the mail server's willingness to take what it mints. Returned
	// through needsReauth, with the cause attached.
	ErrNeedsReauth = &Error{
		Code:    CodeConflict,
		Message: "account needs re-authorization",
	}
)

// needsReauth is ErrNeedsReauth carrying what led to it, for the log. The
// caller sees exactly what ErrNeedsReauth says, and errors.Is still finds
// both it and the cause.
func needsReauth(cause error) error {
	return &Error{Code: ErrNeedsReauth.Code, Message: ErrNeedsReauth.Message, Err: cause}
}
