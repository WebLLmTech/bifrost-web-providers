package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type APIError struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *APIError) Error() string { return e.Message }
func (e *APIError) Unwrap() error { return e.Cause }
func Invalid(message string) error {
	return &APIError{Status: 400, Type: "invalid_request_error", Code: "invalid_request", Message: message}
}
func Unauthorized(message string) error {
	return &APIError{Status: 401, Type: "authentication_error", Code: "invalid_credentials", Message: message}
}
func Unsupported(message string) error {
	return &APIError{Status: 400, Type: "invalid_request_error", Code: "unsupported_operation", Message: message}
}
func Upstream(status int, code, message string) error {
	if status < 400 || status > 599 {
		status = 502
	}
	return &APIError{Status: status, Type: "upstream_error", Code: code, Message: message}
}
func PublicError(err error, secrets ...string) *APIError {
	e := &APIError{Status: 500, Type: "server_error", Code: "internal_error", Message: "Provider request failed"}
	var typed *APIError
	if errors.As(err, &typed) {
		copy := *typed
		e = &copy
		e.Cause = nil
	}
	if errors.Is(err, context.Canceled) {
		e = &APIError{Status: 499, Type: "request_error", Code: "request_canceled", Message: "Request canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		e = &APIError{Status: 504, Type: "request_error", Code: "request_timeout", Message: "Provider request timed out"}
	}
	if e.Status < 400 || e.Status > 599 {
		e.Status = 500
	}
	for _, s := range secrets {
		if s != "" {
			e.Message = strings.ReplaceAll(e.Message, s, "[redacted]")
		}
	}
	return e
}
func Bearer(value string) (Credential, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[7:])
	}
	if value == "" {
		return Credential{}, Unauthorized("Provider credential is required")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return Credential{}, Unauthorized("Invalid provider credential")
	}
	fields := map[string]string{}
	for _, part := range strings.Fields(value) {
		k, v, ok := strings.Cut(part, ":")
		if ok && k != "" {
			if _, dup := fields[k]; dup {
				return Credential{}, Unauthorized("Duplicate credential field")
			}
			fields[k] = v
		}
	}
	return Credential{Value: value, Fields: fields}, nil
}
func ErrorFromStatus(provider string, status int) error {
	return Upstream(status, "upstream_http_error", fmt.Sprintf("%s returned HTTP %d %s", provider, status, http.StatusText(status)))
}
