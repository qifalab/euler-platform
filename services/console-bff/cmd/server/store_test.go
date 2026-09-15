package main

import (
	"errors"
	"testing"

	"github.com/qifalab/euler-platform/errors"
)

// TestE2ptrNonErrorsxDoesNotPanic pins the comma-ok narrowing: a plain error
// (not *errorsx.Error) must be wrapped as an internal error, never panic.
func TestE2ptrNonErrorsxDoesNotPanic(t *testing.T) {
	e := e2ptr(errors.New("plain"))
	if e == nil || e.Code != "Common.InternalError" {
		t.Fatalf("expected wrapped internal error, got %+v", e)
	}

	orig := errorsx.New("Some.Code", errorsx.StatusBadRequest, "msg")
	if got := e2ptr(orig); got != orig {
		t.Fatalf("errorsx error should pass through unchanged")
	}

	if e := e2ptr(nil); e == nil || e.Code != "Common.InternalError" {
		t.Fatalf("nil error should still produce an internal error, got %+v", e)
	}
}
