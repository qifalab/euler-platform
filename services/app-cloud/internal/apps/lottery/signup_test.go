package lottery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
)

func TestSignupWeAuthBindsSiteActionAndIPAndConsumesOnce(t *testing.T) {
	m, s := fixture(t)
	m.rt.ApplicationEnabled = func(context.Context, string, string, string) (bool, error) { return true, nil }
	if _, err := m.rt.DB.Exec("INSERT INTO installations VALUES('weauth','tenant','project','weauth','enabled')"); err != nil {
		t.Fatal(err)
	}
	auth := weauth.New(m.rt)
	if err := auth.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := s
	ws.ApplicationID = "weauth"
	ws.InstallationID = "weauth"
	w := call(auth.Handler(), &ws, "POST", "/sites", `{"name":"活动防刷","domains":["euler.example"],"baseDifficulty":1,"maxDifficulty":2,"batchModeEnabled":false}`)
	status(t, w, 201)
	var site struct {
		ID      string `json:"id"`
		Sitekey string `json:"sitekey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	v := newRoom(t, m, s)
	other := newRoom(t, m, s)
	for _, r := range []room{v, other} {
		status(t, call(m.Handler(), &s, "PUT", "/rooms/"+r.ID+"/signup-policy", fmt.Sprintf(`{"weauthSiteId":%q}`, site.ID)), 200)
	}
	issue := func(action string) string {
		t.Helper()
		w := call(auth.PublicHandler(), nil, "POST", "/pow/challenge", fmt.Sprintf(`{"sitekey":%q,"origin":"https://euler.example","action":%q}`, site.Sitekey, action))
		status(t, w, 200)
		var proof struct {
			ID         string `json:"challenge_id"`
			Seed       string `json:"challenge"`
			Difficulty int    `json:"difficulty"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &proof); err != nil {
			t.Fatal(err)
		}
		var nonce string
		for i := 0; i < 1000000; i++ {
			candidate := fmt.Sprint(i)
			sum := sha256.Sum256([]byte(proof.Seed + candidate))
			if strings.HasPrefix(hex.EncodeToString(sum[:]), strings.Repeat("0", proof.Difficulty)) {
				nonce = candidate
				break
			}
		}
		if nonce == "" {
			t.Fatal("proof not solved")
		}
		w = call(auth.PublicHandler(), nil, "POST", "/pow/verify", fmt.Sprintf(`{"challenge_id":%q,"nonce":%q,"batch_mode":false}`, proof.ID, nonce))
		status(t, w, 200)
		var result struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Token
	}
	token := issue("lottery:" + v.ID)
	// A valid PoW token for a different activity cannot satisfy this signup.
	status(t, call(m.Handler(), &s, "POST", "/rooms/"+other.ID+"/signup", fmt.Sprintf(`{"name":"错误活动","weauthToken":%q}`, token)), 400)
	request := httptest.NewRequest("POST", "/rooms/"+v.ID+"/signup", strings.NewReader(fmt.Sprintf(`{"name":"错误来源","weauthToken":%q}`, token)))
	request.RemoteAddr = "203.0.113.99:1234"
	request = request.WithContext(appkit.WithScope(request.Context(), s))
	w = httptest.NewRecorder()
	m.Handler().ServeHTTP(w, request)
	status(t, w, 400)
	status(t, call(m.Handler(), &s, "POST", "/rooms/"+v.ID+"/signup", fmt.Sprintf(`{"name":"有效报名","weauthToken":%q}`, token)), 201)
	second := s
	second.ActorID = "other"
	status(t, call(m.Handler(), &second, "POST", "/rooms/"+v.ID+"/signup", fmt.Sprintf(`{"name":"重放凭据","weauthToken":%q}`, token)), 400)
	// Anonymous remains supported when qualification/login are not configured.
	inv := call(m.Handler(), &s, "GET", "/rooms/"+other.ID+"/invitation", "")
	status(t, inv, 200)
	var i map[string]string
	if err := json.Unmarshal(inv.Body.Bytes(), &i); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(i["signupURL"], "/")
	join := parts[len(parts)-1]
	status(t, call(m.PublicHandler(), nil, http.MethodPost, "/join/"+join+"/register", `{"name":"缺失验证"}`), 400)
	valid := issue("lottery:" + other.ID)
	status(t, call(m.PublicHandler(), nil, http.MethodPost, "/join/"+join+"/register", fmt.Sprintf(`{"name":"匿名验证","weauthToken":%q}`, valid)), 201)
}
