package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/platform"
)

type opsIdentity struct {
	principal identity.Principal
	err       error
}

func TestMaintenanceRunsImmediatelyRetriesAndCancelsInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMaintenance(ctx, time.Millisecond, "statistics", func(work context.Context) error {
			if _, ok := work.Deadline(); !ok {
				t.Error("maintenance must have deadline")
			}
			if calls.Add(1) == 1 {
				return errors.New("transient failure")
			}
			close(started)
			<-work.Done()
			return work.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not retry")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected maintenance calls", calls.Load())
	}
}

func (a opsIdentity) Authenticate(*http.Request) (identity.Principal, identity.Session, error) {
	return a.principal, identity.Session{}, a.err
}
func (a opsIdentity) ValidateCSRF(*http.Request, identity.Session) error { return nil }

func TestOperationsAccessRequiresExactAdministratorOrDedicatedBearer(t *testing.T) {
	admins := []platform.Administrator{{Provider: "https://identity.example.test", Subject: "123"}}
	token := strings.Repeat("t", 32)
	for _, tc := range []struct {
		name   string
		auth   platform.Authenticator
		bearer string
		want   int
	}{
		{"anonymous", nil, "", 401},
		{"invalid_session", opsIdentity{err: errors.New("expired")}, "", 401},
		{"same_subject_other_issuer", opsIdentity{principal: identity.Principal{Provider: "https://other.example.test", Subject: "123"}}, "", 403},
		{"member", opsIdentity{principal: identity.Principal{Provider: admins[0].Provider, Subject: "456"}}, "", 403},
		{"admin", opsIdentity{principal: identity.Principal{Provider: admins[0].Provider, Subject: admins[0].Subject}}, "", 0},
		{"token", nil, "Bearer " + token, 0},
		{"wrong_token", nil, "Bearer " + strings.Repeat("a", 32), 401},
		{"wrong_scheme", nil, "Basic " + token, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/ops/diagnostics?token="+token, nil)
			r.Header.Set("X-Platform-Role", "admin")
			if tc.bearer != "" {
				r.Header.Set("Authorization", tc.bearer)
			}
			if got := operationsAuthorizer(tc.auth, admins, token)(r); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	r := httptest.NewRequest("GET", "/ops/metrics", nil)
	r.Header.Add("Authorization", "Bearer "+token)
	r.Header.Add("Authorization", "Bearer "+token)
	if operationsAuthorizer(nil, admins, token)(r) != 401 {
		t.Fatal("duplicate authorization accepted")
	}
	r = httptest.NewRequest("GET", "/ops/metrics", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if operationsAuthorizer(nil, admins, "")(r) != 401 {
		t.Fatal("unset token authorized")
	}
}
