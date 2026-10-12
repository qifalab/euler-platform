package eid

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type memberOperation struct {
	scope  appkit.Scope
	method string
	path   string
	body   any
}

func startMemberOperation(t *testing.T, f *testFixture, op memberOperation) <-chan *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(op.body)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r := httptest.NewRequest(op.method, op.path, bytes.NewReader(body))
		r = r.WithContext(appkit.WithScope(ctx, op.scope))
		w := httptest.NewRecorder()
		f.m.Handler().ServeHTTP(w, r)
		done <- w
	}()
	return done
}

// Pause the first request after reading its encrypted row. The second request
// either commits (the old, unsafe behavior), or waits for the first transaction.
// In both cases the final assertion tests the persisted data, not lock timing.
func interleaveMemberOperations(t *testing.T, f *testFixture, readNumber int32, first, second memberOperation) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	defer release()
	decrypt := f.rt.Decrypt
	var reads atomic.Int32
	f.rt.Decrypt = func(b []byte, aad string) (string, error) {
		plain, err := decrypt(b, aad)
		if aad == "eid:"+first.scope.InstallationID+":member:alice" && reads.Add(1) == readNumber {
			close(entered)
			<-resume
		}
		return plain, err
	}
	firstDone := startMemberOperation(t, f, first)
	select {
	case <-entered:
	case response := <-firstDone:
		t.Fatalf("first request completed before controlled read: %d %s", response.Code, response.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not read the member")
	}
	waits := f.rt.DB.Stats().WaitCount
	secondDone := startMemberOperation(t, f, second)
	var secondResponse *httptest.ResponseRecorder
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
waiting:
	for {
		select {
		case secondResponse = <-secondDone:
			break waiting
		case <-ticker.C:
			if f.rt.DB.Stats().WaitCount > waits {
				break waiting
			}
		case <-deadline.C:
			t.Fatal("second request neither finished nor waited for the member transaction")
		}
	}
	release()
	status(t, <-firstDone, 200)
	if secondResponse == nil {
		secondResponse = <-secondDone
	}
	status(t, secondResponse, 200)
}

func TestProfileReadDoesNotOverwriteConcurrentEdit(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	status(t, call(t, f.m, alice, "PATCH", "/profile", map[string]any{"bio": "original", "phone": "old"}), 200)
	interleaveMemberOperations(t, f, 1,
		memberOperation{alice, "GET", "/profile", nil},
		memberOperation{alice, "PATCH", "/profile", map[string]any{"bio": "updated", "phone": "new"}},
	)
	p, err := f.m.member(context.Background(), alice, alice.ActorID)
	if err != nil || p.Bio != "updated" || p.Phone != "new" {
		t.Fatalf("profile read overwrote committed edit: %+v, %v", p, err)
	}
}

func TestProfileEditPreservesConcurrentIdentityRefresh(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	status(t, call(t, f.m, alice, "GET", "/profile", nil), 200)
	refreshed := alice
	refreshed.Email, refreshed.EmailVerified = "new-email@example.test", false
	interleaveMemberOperations(t, f, 2,
		memberOperation{alice, "PATCH", "/profile", map[string]any{"bio": "updated", "phone": "new"}},
		memberOperation{refreshed, "GET", "/profile", nil},
	)
	p, err := f.m.member(context.Background(), alice, alice.ActorID)
	if err != nil || p.Bio != "updated" || p.Phone != "new" || p.Email != refreshed.Email || p.EmailVerified {
		t.Fatalf("profile edit lost refreshed identity fields: %+v, %v", p, err)
	}
}

func TestAdminMemberEditPreservesConcurrentIdentityRefresh(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	operator := actor("operator", "read", "admin")
	status(t, call(t, f.m, alice, "GET", "/profile", nil), 200)
	refreshed := alice
	refreshed.Email, refreshed.EmailVerified = "new-email@example.test", false
	interleaveMemberOperations(t, f, 1,
		memberOperation{operator, "PATCH", "/admin/members/alice", map[string]any{"bio": "operator edit", "phone": "new"}},
		memberOperation{refreshed, "GET", "/profile", nil},
	)
	p, err := f.m.member(context.Background(), alice, alice.ActorID)
	if err != nil || p.Bio != "operator edit" || p.Phone != "new" || p.Email != refreshed.Email || p.EmailVerified {
		t.Fatalf("admin edit lost refreshed identity fields: %+v, %v", p, err)
	}
	status(t, call(t, f.m, operator, "PATCH", "/admin/members/alice", map[string]any{"identityLevel": 2, "identityTitle": "manual"}), 403)
	p, err = f.m.member(context.Background(), alice, alice.ActorID)
	if err != nil || p.Bio != "operator edit" || p.IdentityLevel != 0 || p.IdentityTitle != "" {
		t.Fatalf("rejected qualification edit mutated member: %+v, %v", p, err)
	}
}

func TestProfileWriteRollsBackIdentityRefreshWhenAuditFails(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	status(t, call(t, f.m, alice, "PATCH", "/profile", map[string]any{"bio": "original"}), 200)
	if _, err := f.rt.DB.Exec("CREATE TRIGGER reject_profile_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;"); err != nil {
		t.Fatal(err)
	}
	refreshed := alice
	refreshed.Email = "new-email@example.test"
	status(t, call(t, f.m, refreshed, "PATCH", "/profile", map[string]any{"bio": "must roll back"}), 500)
	p, err := f.m.member(context.Background(), alice, alice.ActorID)
	if err != nil || p.Bio != "original" || p.Email != alice.Email {
		t.Fatalf("failed profile update partially committed identity refresh: %+v, %v", p, err)
	}
}
