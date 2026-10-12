package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/storage/sqltest"
)

// sqlReplicas builds n facades on one support_db: replicas of the service, or
// one process before and after a restart.
func sqlReplicas(t *testing.T, n int) []*quotaStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-quota"))
	out := make([]*quotaStore, n)
	for i := range out {
		s, err := newSQLQuotaStore(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = s
	}
	return out
}

type quotaEnv struct {
	Code string         `json:"Code"`
	Data map[string]any `json:"Data"`
}

func call(t *testing.T, h http.HandlerFunc, path string, acct string, body any) (int, quotaEnv) {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set(accountIDHeader, acct)
	rr := httptest.NewRecorder()
	h(rr, r)
	var env quotaEnv
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	return rr.Code, env
}

func usedOf(t *testing.T, s *quotaStore) (int, int) {
	t.Helper()
	u, err := s.GetUsage(100123, "quota_euecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	return u.Used, u.Occupying
}

// stores runs a test against the in-memory facade (one process) and two
// facades on one MySQL database (two replicas).
func stores(t *testing.T, fn func(t *testing.T, replicas []*quotaStore)) {
	t.Run("memory", func(t *testing.T) {
		s := newInMemoryQuotaStore()
		fn(t, []*quotaStore{s, s})
	})
	t.Run("mysql", func(t *testing.T) { fn(t, sqlReplicas(t, 2)) })
}

// The key outlives the process that saw it and the token it produced.
func TestOccupyKeyReplaysAcrossReplicasAndAfterCommit(t *testing.T) {
	stores(t, func(t *testing.T, rs []*quotaStore) {
		req := occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 3, BizKey: "order-7", IdempotencyKey: "ord-7-occupy"}
		code, first := call(t, rs[0].handleOccupy, "/api/v1/quota/occupy", "100123", req)
		if code != 200 || first.Data["replayed"] != false || first.Data["active"] != true {
			t.Fatalf("first occupy: %d %+v", code, first)
		}
		id := first.Data["reservationId"]
		code, again := call(t, rs[1].handleOccupy, "/api/v1/quota/occupy", "100123", req)
		if code != 200 || again.Data["reservationId"] != id || again.Data["replayed"] != true || again.Data["active"] != true {
			t.Fatalf("replay on the other replica: %d %+v", code, again)
		}
		if again.Data["expiresAt"] != first.Data["expiresAt"] {
			t.Fatalf("replayed expiresAt %v, first %v", again.Data["expiresAt"], first.Data["expiresAt"])
		}
		if code, env := call(t, rs[1].handleCommit, "/api/v1/quota/commit", "100123", map[string]any{"reservationId": id}); code != 200 {
			t.Fatalf("commit: %d %+v", code, env)
		}
		// After the commit the token is gone; the key still names it and
		// nothing is reserved again.
		code, late := call(t, rs[0].handleOccupy, "/api/v1/quota/occupy", "100123", req)
		if code != 200 || late.Data["reservationId"] != id || late.Data["active"] != false {
			t.Fatalf("replay after commit: %d %+v", code, late)
		}
		if used, occ := usedOf(t, rs[0]); used != 3 || occ != 0 {
			t.Fatalf("used/occupying = %d/%d, want 3/0", used, occ)
		}
		// The same key for a different request is a conflict, not a replay.
		req.Count = 4
		if code, env := call(t, rs[0].handleOccupy, "/api/v1/quota/occupy", "100123", req); code != 409 || env.Code != "Quota.IdempotencyConflict" {
			t.Fatalf("reused key: %d %+v", code, env)
		}
	})
}

func TestOccupyKeyConcurrentAcrossReplicas(t *testing.T) {
	stores(t, func(t *testing.T, rs []*quotaStore) {
		req := occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 2, IdempotencyKey: "race"}
		ids := make([]any, 12)
		var wg sync.WaitGroup
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				code, env := call(t, rs[i%2].handleOccupy, "/api/v1/quota/occupy", "100123", req)
				if code != 200 {
					t.Errorf("attempt %d: %d %+v", i, code, env)
				}
				ids[i] = env.Data["reservationId"]
			}(i)
		}
		wg.Wait()
		for i := range ids {
			if ids[i] != ids[0] {
				t.Fatalf("attempts got different reservations: %v", ids)
			}
		}
		if _, occ := usedOf(t, rs[0]); occ != 2 {
			t.Fatalf("Occupying = %d, want 2 (one reservation)", occ)
		}
	})
}

func TestFreeReturnsCommittedUsageOnce(t *testing.T) {
	stores(t, func(t *testing.T, rs []*quotaStore) {
		code, env := call(t, rs[0].handleOccupy, "/api/v1/quota/occupy", "100123", occupyRequest{ProductCode: "euecs", Region: "cn-north-1", Count: 3})
		if code != 200 {
			t.Fatalf("occupy: %d %+v", code, env)
		}
		if code, env := call(t, rs[0].handleCommit, "/api/v1/quota/commit", "100123", map[string]any{"reservationId": env.Data["reservationId"]}); code != 200 {
			t.Fatalf("commit: %d %+v", code, env)
		}

		free := map[string]any{"productCode": "euecs", "resourceType": "instance", "region": "cn-north-1", "amount": 1, "bizKey": "i-abc"}
		var wg sync.WaitGroup
		var mu sync.Mutex
		fresh := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				code, env := call(t, rs[i%2].handleFree, "/internal/quota/free", "100123", free)
				if code != 200 || env.Data["freed"] != true {
					t.Errorf("free %d: %d %+v", i, code, env)
					return
				}
				if env.Data["replayed"] == false {
					mu.Lock()
					fresh++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if fresh != 1 {
			t.Fatalf("%d frees applied, want 1", fresh)
		}
		if used, _ := usedOf(t, rs[1]); used != 2 {
			t.Fatalf("Used = %d, want 2", used)
		}

		// An explicit key, the count alias, and the validation gates.
		if code, env := call(t, rs[1].handleFree, "/internal/quota/free", "100123",
			map[string]any{"productCode": "euecs", "region": "cn-north-1", "count": 5, "idempotencyKey": "rel-2"}); code != 200 {
			t.Fatalf("free by key: %d %+v", code, env)
		}
		if used, _ := usedOf(t, rs[0]); used != 0 {
			t.Fatalf("Used = %d, want 0 (floored, never negative)", used)
		}
		for name, body := range map[string]map[string]any{
			"no key":        {"productCode": "euecs", "amount": 1},
			"no amount":     {"productCode": "euecs", "bizKey": "x"},
			"no product":    {"amount": 1, "bizKey": "x"},
			"alias differs": {"productCode": "euecs", "amount": 1, "count": 2, "bizKey": "x"},
		} {
			if code, env := call(t, rs[0].handleFree, "/internal/quota/free", "100123", body); code != 400 {
				t.Fatalf("%s: %d %+v, want 400", name, code, env)
			}
		}
		if code, env := call(t, rs[0].handleFree, "/internal/quota/free", "100123",
			map[string]any{"productCode": "euecs", "region": "cn-north-1", "amount": 2, "bizKey": "i-abc"}); code != 409 || env.Code != "Quota.IdempotencyConflict" {
			t.Fatalf("same resource, other amount: %d %+v", code, env)
		}
		if code, _ := call(t, rs[0].handleFree, "/internal/quota/free", "", free); code != 403 {
			t.Fatalf("free without account header: %d, want 403", code)
		}
	})
}
