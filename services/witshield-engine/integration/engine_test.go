package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/agent"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/identity"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/secret"
)

func management(t *testing.T, e *Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeManagement(w, r, "euler_actor")
	return w
}
func TestNativeEngineDeviceSignaturesIsolationAndApproval(t *testing.T) {
	ctx := context.Background()
	var enabled atomic.Bool
	enabled.Store(true)
	cfg := Config{DataDir: t.TempDir(), Key: bytes.Repeat([]byte{1}, 32), Enabled: func(context.Context) bool { return enabled.Load() }}
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(ctx, Config{DataDir: t.TempDir(), Key: bytes.Repeat([]byte{2}, 32), Enabled: func(context.Context) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Both projects may configure the same provider, but encrypted credentials
	// remain bound to separate engine keys and persistence.
	settings := management(t, a, "PUT", "/ai/settings", map[string]any{"protocol": "openai_responses", "baseUrl": "https://api.example.test/v1", "model": "test", "apiKey": "project-a-only-secret"})
	if settings.Code != 200 {
		t.Fatalf("AI settings %d %s", settings.Code, settings.Body)
	}
	stored, err := a.db.AISettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherVault, err := secret.New(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = otherVault.Decrypt(stored.EncryptedAPIKey); err == nil {
		t.Fatal("another project decrypted the API key")
	}
	if settings = management(t, b, "GET", "/ai/settings", nil); strings.Contains(settings.Body.String(), "project-a-only-secret") || strings.Contains(settings.Body.String(), "api.example.test") {
		t.Fatal("project AI configuration leaked")
	}
	prefixA := "/public/witshield/tenant/project_a/install_a"
	prefixB := "/public/witshield/tenant/project_b/install_b"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, prefix := a, prefixA
		if strings.HasPrefix(r.URL.Path, prefixB+"/") {
			e, prefix = b, prefixB
		}
		original := r.RequestURI
		u := *r.URL
		u.Path = strings.TrimPrefix(r.URL.Path, prefix)
		clone := r.Clone(r.Context())
		clone.URL = &u
		e.ServeAgent(w, clone, original)
	}))
	defer server.Close()
	w := management(t, a, "POST", "/enrollment-tokens", map[string]any{"name": "Euler device", "expiresIn": "15m"})
	if w.Code != 201 {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	var enrollment struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := agent.NewIdentity()
	client, err := agent.NewObserverClient(server.URL+prefixA, "")
	if err != nil {
		t.Fatal(err)
	}
	device, token, err := client.Enroll(ctx, agent.EnrollRequest{EnrollmentToken: enrollment.Token, Name: "host", Hostname: "host", OS: "linux", Arch: "amd64", AgentVersion: "euler-test", IdentityPublicKey: pub, IdentityPrivateKey: priv, ScanInterval: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Heartbeat(ctx, map[string]string{"name": "host", "hostname": "host", "os": "linux", "arch": "amd64", "agentVersion": "euler-test"}); err != nil {
		t.Fatalf("external path signature failed: %v", err)
	}
	if w = management(t, b, "GET", "/devices/"+device.ID, nil); w.Code != 404 {
		t.Fatalf("cross-project device: %d %s", w.Code, w.Body)
	}
	if w = management(t, b, "POST", "/actions", map[string]any{"deviceId": device.ID, "type": "package_security_upgrade", "parameters": map[string]any{"packages": []string{"openssl"}}}); w.Code != 404 {
		t.Fatalf("cross-project action: %d", w.Code)
	}
	// A signature bound to the full URI must not be accepted under another
	// project, under a stripped route, or twice with the same nonce.
	raw := []byte(`{"name":"host","hostname":"host","os":"linux","arch":"amd64","agentVersion":"euler-test"}`)
	uri := prefixA + "/agent/v1/heartbeat"
	timestamp := fmt.Sprint(time.Now().UnixMilli())
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 18))
	sig, _ := identity.SignAgentRequest(priv, identity.AgentRequestProof{DeviceID: device.ID, Method: "POST", RequestURI: uri, Timestamp: timestamp, Nonce: nonce, Body: raw})
	signed := func(path, signature string) int {
		r, _ := http.NewRequest("POST", server.URL+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-WitShield-Timestamp", timestamp)
		r.Header.Set("X-WitShield-Nonce", nonce)
		r.Header.Set("X-WitShield-Signature", signature)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := signed(prefixB+"/agent/v1/heartbeat", sig); code != 401 {
		t.Fatalf("cross-project signature status=%d", code)
	}
	stripped, _ := identity.SignAgentRequest(priv, identity.AgentRequestProof{DeviceID: device.ID, Method: "POST", RequestURI: "/agent/v1/heartbeat", Timestamp: timestamp, Nonce: nonce, Body: raw})
	if code := signed(uri, stripped); code != 401 {
		t.Fatalf("stripped signature status=%d", code)
	}
	if code := signed(uri, sig); code != 200 {
		t.Fatalf("valid signature status=%d", code)
	}
	if code := signed(uri, sig); code != 401 {
		t.Fatalf("replayed signature status=%d", code)
	}
	w = management(t, a, "POST", "/actions", map[string]any{"deviceId": device.ID, "type": "package_security_upgrade", "parameters": map[string]any{"packages": []string{"openssl"}}})
	if w.Code != 201 {
		t.Fatalf("prepare: %d %s", w.Code, w.Body)
	}
	var prepared struct {
		Action struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"action"`
		Nonce string `json:"approvalNonce"`
	}
	json.Unmarshal(w.Body.Bytes(), &prepared)
	if prepared.Action.Status != "draft" || prepared.Nonce == "" {
		t.Fatal("prepare skipped review/nonce")
	}
	approvePath := "/actions/" + prepared.Action.ID + "/approve"
	if w = management(t, b, "GET", "/actions/"+prepared.Action.ID, nil); w.Code != 404 {
		t.Fatalf("cross-project action read %d", w.Code)
	}
	if w = management(t, b, "POST", approvePath, map[string]string{"approvalNonce": prepared.Nonce}); w.Code != 404 {
		t.Fatalf("cross-project approval %d", w.Code)
	}

	if w = management(t, a, "POST", approvePath, map[string]string{"approvalNonce": "invalid"}); w.Code < 400 {
		t.Fatal("invalid nonce approved")
	}
	if w = management(t, a, "POST", approvePath, map[string]string{"approvalNonce": prepared.Nonce}); w.Code != 202 {
		t.Fatalf("approval: %d %s", w.Code, w.Body)
	}
	if w = management(t, a, "GET", "/actions/"+prepared.Action.ID, nil); !strings.Contains(w.Body.String(), `"approvedBy":"euler_actor"`) {
		t.Fatalf("approval actor not retained %s", w.Body)
	}
	if w = management(t, a, "POST", approvePath, map[string]string{"approvalNonce": prepared.Nonce}); w.Code < 400 {
		t.Fatal("nonce reused")
	}
	for _, path := range []string{"/admin/bootstrap", "/auth/login", "/auth/me", "/status"} {
		if w = management(t, a, "GET", path, nil); w.Code != 404 {
			t.Fatalf("legacy path exposed: %s %d", path, w.Code)
		}
	}
	enabled.Store(false)
	if err = client.Heartbeat(ctx, map[string]string{"name": "host", "hostname": "host", "os": "linux", "arch": "amd64", "agentVersion": "euler-test"}); err == nil {
		t.Fatal("disabled installation accepted heartbeat")
	}
	if w = management(t, a, "POST", "/devices/"+device.ID+"/scan", map[string]any{}); w.Code != 503 {
		t.Fatalf("disabled scan %d", w.Code)
	}
	enabled.Store(true)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if w = management(t, a, "GET", "/devices/"+device.ID, nil); w.Code != 200 {
		t.Fatalf("restart lost device: %d", w.Code)
	}
	if err = client.Heartbeat(ctx, map[string]string{"name": "host", "hostname": "host", "os": "linux", "arch": "amd64", "agentVersion": "euler-test"}); err != nil {
		t.Fatalf("restart lost credential: %v", err)
	}
}
