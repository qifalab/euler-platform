package lottery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
)

func fixture(t *testing.T) (*Module, appkit.Scope) {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "lottery.db"))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, e = db.Exec(`PRAGMA foreign_keys=ON;
CREATE TABLE installations(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,application_id TEXT,status TEXT);
INSERT INTO installations VALUES('install','tenant','project','lottery','enabled');
CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER);`)
	if e != nil {
		t.Fatal(e)
	}
	m := New(&appkit.Runtime{DB: db, PublicURL: "https://euler.example", Encrypt: func(p, a string) ([]byte, error) { return []byte(p), nil }, Decrypt: func(c []byte, a string) (string, error) { return string(c), nil }, DeriveKey: func(string) []byte { return []byte("test-only-rate-key") }})
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	return m, appkit.Scope{ActorID: "actor", TenantID: "tenant", ProjectID: "project", InstallationID: "install", ApplicationID: "lottery", Permissions: []string{"read", "write", "manage"}}
}
func call(h http.Handler, s *appkit.Scope, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if s != nil {
		r = r.WithContext(appkit.WithScope(r.Context(), *s))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func status(t *testing.T, w *httptest.ResponseRecorder, n int) {
	t.Helper()
	if w.Code != n {
		t.Fatalf("got %d want %d body %s", w.Code, n, w.Body.String())
	}
}
func newRoom(t *testing.T, m *Module, s appkit.Scope) room {
	t.Helper()
	w := call(m.Handler(), &s, "POST", "/rooms", `{"name":"年度庆典","description":"欢迎参与"}`)
	status(t, w, 201)
	var v room
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func batch(t *testing.T, m *Module, s appkit.Scope, v room, n int) {
	t.Helper()
	status(t, call(m.Handler(), &s, "POST", "/rooms/"+v.ID+"/participants/batch", fmt.Sprintf(`{"count":%d,"startFrom":1}`, n)), 201)
}
func drawBody(id string, count int) string {
	return fmt.Sprintf(`{"requestId":%q,"count":%d,"prizeName":"幸运奖","preventDuplicates":true}`, id, count)
}
func TestConcurrentDrawsAreAtomicAndDoNotRepeat(t *testing.T) {
	m, s := fixture(t)
	v := newRoom(t, m, s)
	batch(t, m, s, v, 20)
	h := m.Handler()
	results := make(chan *httptest.ResponseRecorder, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody(fmt.Sprintf("request-%d", i), 2))
		}(i)
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	rounds := map[int]bool{}
	for w := range results {
		status(t, w, 200)
		var d drawResult
		if e := json.Unmarshal(w.Body.Bytes(), &d); e != nil {
			t.Fatal(e)
		}
		if rounds[d.RoundNumber] {
			t.Fatal("duplicate round")
		}
		rounds[d.RoundNumber] = true
		for _, p := range d.Winners {
			if seen[p.ID] {
				t.Fatal("duplicate winner across concurrent draws")
			}
			seen[p.ID] = true
		}
	}
	if len(seen) != 20 || len(rounds) != 10 {
		t.Fatalf("bad counts %d %d", len(seen), len(rounds))
	}
	status(t, call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("exhausted", 1)), 409)
	var draws, audits int
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM lottery_draws").Scan(&draws)
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM audit WHERE action='lottery.draw.completed'").Scan(&audits)
	if draws != 10 || audits != 10 {
		t.Fatalf("draw/audit mismatch %d/%d", draws, audits)
	}
}
func TestConcurrentIdempotencyAndReset(t *testing.T) {
	m, s := fixture(t)
	v := newRoom(t, m, s)
	batch(t, m, s, v, 5)
	h := m.Handler()
	results := make(chan *httptest.ResponseRecorder, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("same-request", 2))
		}()
	}
	wg.Wait()
	close(results)
	id := ""
	for w := range results {
		status(t, w, 200)
		var d drawResult
		json.Unmarshal(w.Body.Bytes(), &d)
		if id != "" && id != d.ID {
			t.Fatal("retry produced a new draw")
		}
		id = d.ID
	}
	status(t, call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("same-request", 3)), 409)
	status(t, call(h, &s, "POST", "/rooms/"+v.ID+"/reset", `{"confirm":"wrong"}`), 400)
	status(t, call(h, &s, "POST", "/rooms/"+v.ID+"/reset", `{"confirm":"`+v.ID+`"}`), 200)
	status(t, call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("same-request", 2)), 409)
	w := call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("new-cycle", 5))
	status(t, w, 200)
	var d drawResult
	json.Unmarshal(w.Body.Bytes(), &d)
	if d.RoundNumber != 1 || len(d.Winners) != 5 {
		t.Fatal("reset lost participants or round counter")
	}
}
func TestBoundariesAndPublicSignup(t *testing.T) {
	m, s := fixture(t)
	v := newRoom(t, m, s)
	h := m.Handler()
	base := "/rooms/" + v.ID
	status(t, call(h, nil, "GET", base, ""), 401)
	viewer := s
	viewer.Permissions = []string{"read"}
	status(t, call(h, &viewer, "POST", base+"/draws", drawBody("viewer-key", 1)), 403)
	other := s
	other.ProjectID = "another"
	for _, op := range []struct{ method, path, body string }{{"GET", base, ""}, {"GET", base + "/participants", ""}, {"GET", base + "/draws", ""}, {"GET", base + "/invitation", ""}, {"POST", base + "/draws", drawBody("cross-project", 1)}, {"POST", base + "/reset", `{"confirm":"` + v.ID + `"}`}} {
		status(t, call(h, &other, op.method, op.path, op.body), 404)
	}
	w := call(h, &s, "GET", base+"/invitation", "")
	status(t, w, 200)
	var invitation map[string]string
	json.Unmarshal(w.Body.Bytes(), &invitation)
	joinPath := strings.TrimPrefix(invitation["signupURL"], "https://euler.example/public/lottery")
	pub := m.PublicHandler()
	w = call(pub, nil, "GET", joinPath, "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), "年度庆典") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "nonce-") {
		t.Fatal("public form missing")
	}
	w = call(pub, nil, "GET", joinPath+"/qr.png", "")
	status(t, w, 200)
	if w.Header().Get("Content-Type") != "image/png" || w.Body.Len() < 100 {
		t.Fatal("QR not generated")
	}
	status(t, call(pub, nil, "POST", joinPath+"/register", `{"name":"张三","department":"研发"}`), 201)
	status(t, call(pub, nil, "POST", joinPath+"/register", `{"name":"张三"}`), 409)
	status(t, call(pub, nil, "POST", joinPath+"/register", `{"name":"李四","projectId":"other"}`), 400)
	status(t, call(pub, nil, "GET", "/rooms", ""), 404)
	status(t, call(pub, nil, "POST", "/reset-db", `{"confirm":"RESET_DATABASE"}`), 404)
	status(t, call(h, &s, "POST", base+"/invitation/rotate", `{}`), 200)
	status(t, call(pub, nil, "POST", joinPath+"/register", `{"name":"李四"}`), 404)
	w = call(h, &s, "GET", base+"/invitation", "")
	json.Unmarshal(w.Body.Bytes(), &invitation)
	joinPath = strings.TrimPrefix(invitation["signupURL"], "https://euler.example/public/lottery")
	if _, e := m.rt.DB.Exec("UPDATE installations SET status='disabled'"); e != nil {
		t.Fatal(e)
	}
	status(t, call(pub, nil, "POST", joinPath+"/register", `{"name":"李四"}`), 404)
}
func TestRollbackAndDeletedWinnerSnapshot(t *testing.T) {
	m, s := fixture(t)
	v := newRoom(t, m, s)
	batch(t, m, s, v, 3)
	h := m.Handler()
	if _, e := m.rt.DB.Exec(`CREATE TRIGGER fail_draw_audit BEFORE INSERT ON audit WHEN NEW.action='lottery.draw.completed' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`); e != nil {
		t.Fatal(e)
	}
	status(t, call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("rollback-key", 1)), 500)
	var count, next int
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM lottery_draws").Scan(&count)
	m.rt.DB.QueryRow("SELECT next_round FROM lottery_rooms").Scan(&next)
	if count != 0 || next != 1 {
		t.Fatal("partial draw committed")
	}
	m.rt.DB.Exec("DROP TRIGGER fail_draw_audit")
	w := call(h, &s, "POST", "/rooms/"+v.ID+"/draws", drawBody("rollback-key", 1))
	status(t, w, 200)
	var d drawResult
	json.Unmarshal(w.Body.Bytes(), &d)
	status(t, call(h, &s, "DELETE", "/rooms/"+v.ID+"/participants/"+d.Winners[0].ID, ""), 200)
	w = call(h, &s, "GET", "/rooms/"+v.ID+"/draws", "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), d.Winners[0].Name) {
		t.Fatal("removing a participant erased the recorded winner")
	}
}

func TestRepeatWinsKeepOneParticipantAndClosedRoomStopsPublicWrites(t *testing.T) {
	m, s := fixture(t)
	v := newRoom(t, m, s)
	batch(t, m, s, v, 1)
	h := m.Handler()
	base := "/rooms/" + v.ID
	for _, requestID := range []string{"repeat-first", "repeat-second"} {
		status(t, call(h, &s, "POST", base+"/draws", `{"count":1,"requestId":"`+requestID+`","preventDuplicates":false}`), 200)
	}
	w := call(h, &s, "GET", base+"/participants", "")
	status(t, w, 200)
	var list struct {
		Participants []participant `json:"participants"`
		Total        int           `json:"total"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &list); e != nil {
		t.Fatal(e)
	}
	if list.Total != 1 || len(list.Participants) != 1 || !list.Participants[0].Participated {
		t.Fatal("repeat wins duplicated or lost participant rows")
	}
	w = call(h, &s, "GET", base+"/invitation", "")
	var link map[string]string
	if e := json.Unmarshal(w.Body.Bytes(), &link); e != nil {
		t.Fatal(e)
	}
	join := strings.TrimPrefix(link["signupURL"], "https://euler.example/public/lottery")
	request := httptest.NewRequest("POST", join+"/register", strings.NewReader(`{"name":"外域报名"}`))
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	m.PublicHandler().ServeHTTP(response, request)
	status(t, response, 403)
	status(t, call(h, &s, "PATCH", base, `{"name":"暂停的活动","status":"closed"}`), 200)
	status(t, call(m.PublicHandler(), nil, "POST", join+"/register", `{"name":"关闭后报名"}`), 409)
	status(t, call(h, &s, "POST", base+"/draws", `{"count":1,"requestId":"closed-draw","preventDuplicates":false}`), 409)
}
