package lottery

import (
	"encoding/json"
	"testing"
)

// PATCH /rooms/{id} is a partial update (service accounts may call it). A
// field the caller omits keeps its stored value: renaming a paused room must
// not reopen it for public signup and draws or clear its description. The
// response describes the room as GET does, including its counts.
func TestPatchRoomKeepsOmittedFieldsAndReportsCounts(t *testing.T) {
	m, s := fixture(t)
	v := newRoom(t, m, s)
	batch(t, m, s, v, 3)
	h := m.Handler()
	base := "/rooms/" + v.ID
	status(t, call(h, &s, "PATCH", base, `{"name":"年度庆典","description":"欢迎参与","status":"closed","preventDuplicates":false}`), 200)
	read := func(body []byte) room {
		t.Helper()
		var got room
		if e := json.Unmarshal(body, &got); e != nil {
			t.Fatal(e)
		}
		return got
	}

	w := call(h, &s, "PATCH", base, `{"name":"新的名称"}`)
	status(t, w, 200)
	got := read(w.Body.Bytes())
	if got.Name != "新的名称" || got.Status != "closed" || got.Description != "欢迎参与" || got.PreventDuplicates {
		t.Fatalf("rename changed other fields: %+v", got)
	}
	if got.TotalUsers != 3 || got.Generation != 1 || got.NextRound != 1 {
		t.Fatalf("PATCH response lost room counts: %+v", got)
	}
	w = call(h, &s, "GET", base, "")
	status(t, w, 200)
	if stored := read(w.Body.Bytes()); stored.Status != "closed" || stored.Description != "欢迎参与" || stored.PreventDuplicates || stored.Name != "新的名称" {
		t.Fatalf("stored room after rename: %+v", stored)
	}
	// A paused room still refuses draws after the rename.
	status(t, call(h, &s, "POST", base+"/draws", `{"count":1,"requestId":"after-rename"}`), 409)

	// Explicit fields still apply and are validated.
	w = call(h, &s, "PATCH", base, `{"status":"open"}`)
	status(t, w, 200)
	if got = read(w.Body.Bytes()); got.Status != "open" || got.Name != "新的名称" || got.Description != "欢迎参与" {
		t.Fatalf("status-only PATCH: %+v", got)
	}
	status(t, call(h, &s, "PATCH", base, `{"status":"paused"}`), 400)
	status(t, call(h, &s, "PATCH", base, `{"name":"   "}`), 400)
	status(t, call(h, &s, "PATCH", base, `{"description":""}`), 200)
	other := s
	other.ProjectID = "another"
	status(t, call(h, &other, "PATCH", base, `{"name":"越权"}`), 404)
}
