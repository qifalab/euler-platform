package errorsx

import (
	"errors"
	"testing"
)

func TestErrorFormatting(t *testing.T) {
	e := ErrQuotaExceeded
	if e.Code != "Quota.Exceeded" || e.HTTPStatus != 403 {
		t.Fatalf("ErrQuotaExceeded wrong: %+v", e)
	}
	if got := e.Error(); got != "Quota.Exceeded: quota exceeded" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestAsFallback(t *testing.T) {
	// A plain error unwraps to the 500 fallback.
	plain := errors.New("boom")
	e := As(plain)
	if e.Code != "Common.InternalError" || e.HTTPStatus != 500 {
		t.Fatalf("As(plain) = %+v", e)
	}
	// An *Error unwraps to itself.
	orig := ErrInvalidParameter
	if As(orig) != orig {
		t.Fatal("As should return the same *Error")
	}
	// nil passes through.
	if As(nil) != nil {
		t.Fatal("As(nil) should be nil")
	}
}

func TestWithRequestID(t *testing.T) {
	e := ErrResourceNotFound.WithRequestID("req-abc")
	if e.RequestID != "req-abc" {
		t.Fatalf("RequestID = %q", e.RequestID)
	}
	// Original must be untouched.
	if ErrResourceNotFound.RequestID != "" {
		t.Fatal("WithRequestID mutated the shared error")
	}
}
