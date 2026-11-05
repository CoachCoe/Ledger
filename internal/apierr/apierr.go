// Package apierr defines the structured errors Ledger returns to callers.
package apierr

import (
	"errors"
	"fmt"
)

// Error is a machine-readable failure. Code is stable across releases;
// Message is for humans; Details carries code-specific fields.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// New returns an Error with the given code and formatted message.
func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// With returns a copy of e carrying an additional detail field.
func (e *Error) With(key string, value any) *Error {
	c := *e
	c.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		c.Details[k] = v
	}
	c.Details[key] = value
	return &c
}

// As converts any error into an Error, wrapping unstructured errors as
// code "internal".
func As(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	return &Error{Code: "internal", Message: err.Error()}
}

// HasCode reports whether err is or wraps an Error with the given code.
func HasCode(err error, code string) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Code == code
}
