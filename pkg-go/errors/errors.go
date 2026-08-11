// Package errorsx defines the platform's unified error model for OpenAPI
// responses and gRPC status details (03-backend-services.md §9.3).
//
// Response body: { "RequestId": "...", "Code": "...", "Message": "...", "Data": {...} }
// HTTP status mapping: 400 param/rule, 403 auth/perm/欠费, 404 not found,
// 409 state conflict, 429 throttling, 500/503 internal/unavailable.
//
// Business error code format: {Product}.{Module}.{Reason} (PascalCase),
// e.g. Quota.Exceeded.ScecsInstance. Codes are registered in svc-api-meta;
// unregistered codes are blocked at CI time (03§9.3).
package errorsx

import (
	"errors"
	"fmt"
)

// HTTP status semantics (03§9.3).
const (
	StatusBadRequest     = 400
	StatusForbidden      = 403
	StatusNotFound       = 404
	StatusConflict       = 409
	StatusTooManyRequests = 429
	StatusInternalError  = 500
	StatusUnavailable    = 503
)

// Error is the platform's canonical business error. It carries the business
// code, HTTP status, user-facing message, and a request id (filled by the
// gateway). Services return *Error from their handlers; the gateway renders
// the JSON body.
type Error struct {
	Code       string // {Product}.{Module}.{Reason}, PascalCase
	HTTPStatus int
	Message    string
	RequestID  string // populated by gateway
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New constructs an Error. Callers should prefer the typed constructors below
// so the code/status pairing stays consistent.
func New(code string, httpStatus int, message string) *Error {
	return &Error{Code: code, HTTPStatus: httpStatus, Message: message}
}

// As unwraps a generic error into *Error, returning a fallback 500 if the
// error is not already an *Error.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New("Common.InternalError", StatusInternalError, err.Error())
}

// Common codes (03§9.3 examples).
var (
	ErrInvalidParameter  = New("Common.InvalidParameter", StatusBadRequest, "invalid parameter")
	ErrThrottling        = New("Common.Throttling", StatusTooManyRequests, "request throttled")
	ErrInternal          = New("Common.InternalError", StatusInternalError, "internal error")
	ErrSignatureMismatch = New("IAM.SignatureDoesNotMatch", StatusForbidden, "signature does not match")
	ErrNoPermission      = New("IAM.NoPermission", StatusForbidden, "no permission")
	ErrInvalidClientToken = New("Order.InvalidClientToken", StatusBadRequest, "idempotency key conflict with different parameters")
	ErrQuotaExceeded     = New("Quota.Exceeded", StatusForbidden, "quota exceeded")
	ErrResourceNotFound  = New("Resource.NotFound", StatusNotFound, "resource not found")
	ErrIncorrectStatus   = New("Resource.IncorrectStatus", StatusConflict, "resource state does not allow this operation")
	ErrInsufficientBalance = New("Billing.InsufficientBalance", StatusForbidden, "insufficient balance")
)

// WithRequestID returns a copy of the error with the request id attached.
func (e *Error) WithRequestID(requestID string) *Error {
	clone := *e
	clone.RequestID = requestID
	return &clone
}
