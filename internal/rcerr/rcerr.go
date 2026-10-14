// Package rcerr carries rcmanager's user-facing errors.
//
// Every refusal a user can act on gets a hint: the message says what went
// wrong, the hint says what to type next.
package rcerr

import "fmt"

// Error is a message plus an optional actionable hint and machine-readable code.
type Error struct {
	Message string
	Hint    string
	Code    string
}

func (e *Error) Error() string { return e.Message }

// New builds an error with a hint. An empty hint is fine.
func New(message, hint string) *Error {
	return &Error{Message: message, Hint: hint, Code: "ERROR"}
}

// Newf is New with formatting on the message.
func Newf(hint, format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...), Hint: hint, Code: "ERROR"}
}

// HintOf returns the hint of err when it carries one.
func HintOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Hint
	}
	return ""
}

// CodeOf returns the code of err, defaulting to ERROR.
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok && e.Code != "" {
		return e.Code
	}
	return "ERROR"
}
