package sdk

import (
	"errors"
	"fmt"
	"net/http"
)

// Error codes. gitopsify picks its advice and exit code from them.
const (
	// CodeInvalidSettings: a setting is missing or malformed.
	CodeInvalidSettings = "invalid-settings"
	// CodeUnauthenticated: the credentials were refused.
	CodeUnauthenticated = "unauthenticated"
	// CodeForbidden: the credentials work but lack a permission.
	CodeForbidden = "forbidden"
	// CodeUnreachable: the tool can't be reached (DNS, TLS, timeouts).
	CodeUnreachable = "unreachable"
	// CodeUnsupported: the tool's version or edition isn't supported, or the
	// request uses a protocol the provider doesn't speak.
	CodeUnsupported = "unsupported"
	// CodeCanceled: gitopsify stopped the provider.
	CodeCanceled = "canceled"
	// CodeInternal: anything else, including bugs.
	CodeInternal = "internal"
)

// Error is a failure a provider reports to gitopsify. Message says what went
// wrong; Hint says what the user can do about it.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return e.Message + " (" + e.Hint + ")"
	}
	return e.Message
}

// Errorf returns an Error with a code and a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint returns a copy of the error with a hint.
func (e *Error) WithHint(format string, args ...any) *Error {
	c := *e
	c.Hint = fmt.Sprintf(format, args...)
	return &c
}

// CodeForHTTPStatus maps an HTTP status to an error code.
func CodeForHTTPStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return CodeUnauthenticated
	case status == http.StatusForbidden:
		return CodeForbidden
	case status == http.StatusNotImplemented:
		return CodeUnsupported
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return CodeUnreachable
	default:
		return CodeInternal
	}
}

// AsError converts any error into an *Error, keeping the code of a wrapped
// *Error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}
