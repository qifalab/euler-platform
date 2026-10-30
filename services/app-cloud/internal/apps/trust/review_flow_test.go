package trust

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The existing validation test only references a material ID that does not
// exist, so the owner and scheme checks in validateData were never exercised.
func TestSubmissionRejectsMaterialsOfAnotherActorOrScheme(t *testing.T) {
	f := newFixture(t)
	first := schemeFixture(t, f)
	second := schemeFixture(t, f)
	alice := person("alice", "read", "write")
	bob := person("bob", "read", "write")
	bobs := upload(t, f, bob, first)
	own := upload(t, f, alice, first)
	submitWith := func(scheme Scheme, material string) int {
		return request(t, f.m, alice, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "version": 0, "data": map[string]any{"full_name": "Alice", "zero": 0, "proof": material}}).Code
	}
	if code := submitWith(first, bobs.ID); code != 400 {
		t.Fatalf("another actor's material accepted: %d", code)
	}
	if code := submitWith(second, own.ID); code != 400 {
		t.Fatalf("material of another scheme accepted: %d", code)
	}
	if code := submitWith(first, own.ID); code != 201 {
		t.Fatalf("own material refused: %d", code)
	}
}

func TestKeyExpiryWindowAndExpiredKey(t *testing.T) {
	f := newFixture(t)
	scheme := schemeFixture(t, f)
	alice := person("alice", "read", "write")
	expect(t, request(t, f.m, alice, "GET", "/bootstrap", nil), 200)
	admin := person("admin", "read", "admin", "secrets")
	spec := func(expires time.Time) APIKey {
		return APIKey{Name: "状态", SchemeIDs: []string{scheme.ID}, ActorIDs: []string{alice.ActorID}, ExpiresAt: expires.Format(time.RFC3339)}
	}
	expect(t, request(t, f.m, admin, "POST", "/keys", spec(time.Now().Add(-time.Minute))), 400)
	expect(t, request(t, f.m, admin, "POST", "/keys", spec(time.Now().Add(91*24*time.Hour))), 400)
	w := request(t, f.m, admin, "POST", "/keys", spec(time.Now().Add(time.Hour)))
	expect(t, w, 201)
	created := decode[struct {
		Key   APIKey `json:"key"`
		Token string `json:"token"`
	}](t, w)
	status := func() int {
		r := httptest.NewRequest("GET", "/verification/status?actorId=alice&schemeId="+scheme.ID, nil)
		r.Header.Set("Authorization", "Bearer "+created.Token)
		rec := httptest.NewRecorder()
		f.m.PublicHandler().ServeHTTP(rec, r)
		return rec.Code
	}
	if code := status(); code != 200 {
		t.Fatalf("valid key refused: %d", code)
	}
	if _, e := f.rt.DB.Exec("UPDATE trust_keys SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), created.Key.ID); e != nil {
		t.Fatal(e)
	}
	if code := status(); code != 401 {
		t.Fatalf("expired key accepted: %d", code)
	}
}

func TestPublicMaterialOnlyServesCurrentApprovedFile(t *testing.T) {
	f := newFixture(t)
	scheme := schemeFixture(t, f)
	alice := person("alice", "read", "write")
	reviewer := person("reviewer", "read", "admin", "review", "secrets")
	old := upload(t, f, alice, scheme)
	sub := submitFixture(t, f, scheme, alice, 0, "Alice", old.ID)
	w := request(t, f.m, reviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": sub.Version, "status": "rejected", "reason": "请更换证明"})
	expect(t, w, 200)
	rejected := decode[Submission](t, w)
	current := upload(t, f, alice, scheme)
	resub := submitFixture(t, f, scheme, alice, rejected.Version, "Alice", current.ID)
	w = request(t, f.m, reviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": resub.Version, "status": "approved"})
	expect(t, w, 200)
	approved := decode[Submission](t, w)
	w = request(t, f.m, reviewer, "POST", "/keys", APIKey{Name: "详情", SchemeIDs: []string{scheme.ID}, ActorIDs: []string{alice.ActorID}, Details: true, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)})
	expect(t, w, 201)
	token := decode[struct {
		Token string `json:"token"`
	}](t, w).Token
	material := func(id string) int {
		r := httptest.NewRequest("GET", "/materials/"+id, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.m.PublicHandler().ServeHTTP(rec, r)
		return rec.Code
	}
	if code := material(old.ID); code != 404 {
		t.Fatalf("superseded material served: %d", code)
	}
	if code := material(current.ID); code != 200 {
		t.Fatalf("current material refused: %d", code)
	}
	expect(t, request(t, f.m, reviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": approved.Version, "status": "rejected", "reason": "改判"}), 200)
	if code := material(current.ID); code != 403 {
		t.Fatalf("material of a no longer approved submission served: %d", code)
	}
}
