package contentstore

import "fmt"

// Kind classifies an engine error so a transport binding can map it to its own
// error model (e.g. an HTTP status or a gotsrpc ServiceError type) without the
// core depending on that model.
type Kind int

const (
	// KindInternal is an unexpected/technical failure (storage, serialization).
	KindInternal Kind = iota
	// KindBadRequest is a caller/validation error (bad input, unknown type/field).
	KindBadRequest
	// KindNotFound is a missing item addressed by an id/key match.
	KindNotFound
	// KindNotAcceptable is a state precondition failure (e.g. nothing to publish,
	// or a required field missing on publish).
	KindNotAcceptable
	// KindConflict is an optimistic-concurrency or create-only precondition failure.
	KindConflict
	// KindForbidden is an operation disallowed by the immutable deployment role.
	KindForbidden
)

// Error is the typed error returned by the engine. Kind lets callers map it to
// their transport's error model; Msg is a caller-safe message; Err wraps an
// underlying cause (nil for validation/state errors).
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func badRequest(msg string) *Error    { return &Error{Kind: KindBadRequest, Msg: msg} }
func notFound(msg string) *Error      { return &Error{Kind: KindNotFound, Msg: msg} }
func notAcceptable(msg string) *Error { return &Error{Kind: KindNotAcceptable, Msg: msg} }
func conflict(msg string) *Error      { return &Error{Kind: KindConflict, Msg: msg} }
func forbidden(msg string) *Error     { return &Error{Kind: KindForbidden, Msg: msg} }

func internalErr(err error, msg string) *Error {
	return &Error{Kind: KindInternal, Msg: msg, Err: err}
}
