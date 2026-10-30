package eid

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

func TestInstallationLockDoesNotBlockOtherProjects(t *testing.T) {
	f := fixture(t)
	owner := actor("owner", "read", "write", "manage", "secrets")
	other := owner
	other.TenantID, other.ProjectID, other.InstallationID = "team-b", "project-b", "eid-b"
	put := func(s appkit.Scope) chan int {
		r := httptest.NewRequest("PUT", "/settings", bytes.NewReader([]byte("{}")))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(appkit.WithScope(r.Context(), s))
		done := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			f.m.Handler().ServeHTTP(w, r)
			done <- w.Code
		}()
		return done
	}
	// Hold one installation's lock, as a slow SMTP send does.
	unlock := f.m.lock(owner)
	select {
	case <-put(other):
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("another project's settings waited for this installation's lock")
	}
	same := put(owner)
	select {
	case <-same:
		unlock()
		t.Fatal("the same installation bypassed its lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-same:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not resume after the lock was released")
	}
}
