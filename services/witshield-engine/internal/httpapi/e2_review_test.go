// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/ids"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/secret"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/store"
)

// A local provider (http on loopback/private networks is explicitly allowed)
// often needs no API key. Testing such a saved configuration must succeed.
func TestE2StoredAITestWithoutAPIKeySucceeds(t *testing.T) {
	x := newTestAPI(t)
	x.bootstrapAdmin(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer upstream.Close()
	status, body := request(t, x.admin, http.MethodPut, x.server.URL+"/api/v1/ai/settings", map[string]any{"protocol": "openai_chat", "baseUrl": upstream.URL + "/v1", "model": "local-model"}, nil)
	if status != http.StatusOK {
		t.Fatalf("save keyless settings=%d %s", status, body)
	}
	status, body = request(t, x.admin, http.MethodPost, x.server.URL+"/api/v1/ai/test", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("keyless stored AI test=%d %s", status, body)
	}
}

// The brute-force grant is mirrored into the legacy defense policy. Saving it
// must keep the mode the administrator chose: an enabled "observe" grant was
// stored as "assist", which turns on automatic AI investigation.
func TestE2BruteForceGrantStoresRequestedMode(t *testing.T) {
	x := newTestAPI(t)
	x.bootstrapAdmin(t)
	deviceID, _ := x.enroll(t)
	grantURL := x.server.URL + "/api/v1/devices/" + deviceID + "/policy-grants"
	stored := func() domain.PolicyGrant {
		t.Helper()
		status, body := request(t, x.admin, http.MethodGet, grantURL, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("list grants=%d %s", status, body)
		}
		var list struct {
			Items []domain.PolicyGrant `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatal(err)
		}
		for _, item := range list.Items {
			if item.Capability == "network.auth_bruteforce" {
				return item
			}
		}
		t.Fatal("brute-force grant missing")
		return domain.PolicyGrant{}
	}
	for _, tc := range []struct {
		enabled bool
		mode    domain.AutonomyMode
	}{{false, domain.AutonomyAssist}, {true, domain.AutonomyAssist}, {true, domain.AutonomyObserve}} {
		status, body := request(t, x.admin, http.MethodPut, grantURL+"/network.auth_bruteforce", map[string]any{"enabled": tc.enabled, "mode": tc.mode, "allowedActionTypes": []string{"temporary_ip_ban"}, "maxActionsPerHour": 10, "emergencyStop": false}, nil)
		if status != http.StatusOK {
			t.Fatalf("put grant %+v=%d %s", tc, status, body)
		}
		var echoed domain.PolicyGrant
		if err := json.Unmarshal(body, &echoed); err != nil {
			t.Fatal(err)
		}
		got := stored()
		if got.Mode != tc.mode || got.Enabled != tc.enabled || echoed.Mode != got.Mode || echoed.Enabled != got.Enabled {
			t.Fatalf("requested mode=%s enabled=%v; stored mode=%s enabled=%v; response mode=%s enabled=%v", tc.mode, tc.enabled, got.Mode, got.Enabled, echoed.Mode, echoed.Enabled)
		}
	}
	grants, err := x.store.ListPolicyGrants(context.Background(), deviceID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if automaticInvestigationAllowed(domain.Incident{Category: "identity_access", Severity: domain.SeverityHigh}, grants) {
		t.Fatal("an enabled observe grant permits automatic AI investigation")
	}
	policy, err := x.store.DefensePolicy(context.Background(), deviceID, time.Now().UTC())
	if err != nil || !policy.Enabled || policy.AutoBan {
		t.Fatalf("legacy detection policy not kept in sync: %+v err=%v", policy, err)
	}
}

func TestE2CancelledInvestigationReleasesIncident(t *testing.T) {
	x := newTestAPI(t)
	x.bootstrapAdmin(t)
	deviceID, _ := x.enroll(t)
	received := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so net/http can observe the client closing the
		// connection and cancel this handler's context.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case received <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	defer upstream.Close()
	status, body := request(t, x.admin, http.MethodPut, x.server.URL+"/api/v1/ai/settings", map[string]any{"protocol": "openai_chat", "baseUrl": upstream.URL + "/v1", "model": "m", "apiKey": "provider-key-0001"}, nil)
	if status != http.StatusOK {
		t.Fatalf("save settings=%d %s", status, body)
	}
	now := time.Now().UTC()
	if _, err := x.store.ProcessSecurityEvent(context.Background(), domain.SecurityEvent{ID: "evt_e2_runtime", DeviceID: deviceID, Type: "container_runtime_anomaly", OccurredAt: now, Payload: json.RawMessage(`{"container":"api"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	incidents, err := x.store.ListIncidents(context.Background(), deviceID, nil, 10)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%v err=%v", incidents, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-received
		cancel()
	}()
	if _, _, err = x.api.performIncidentInvestigation(ctx, incidents[0].ID, "administrator"); err == nil {
		t.Fatal("cancelled investigation reported success")
	}
	incident, err := x.store.Incident(context.Background(), incidents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	investigations, err := x.store.ListInvestigations(context.Background(), incidents[0].ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status != domain.IncidentOpen || len(investigations) != 1 || investigations[0].Status != domain.InvestigationFailed {
		t.Fatalf("cancelled investigation left incident=%s investigations=%+v", incident.Status, investigations)
	}
}

// A shutdown that cancels the worker while it is about to claim from the outbox
// must not be reported as an outbox failure (error log + degraded health).
func TestE2NotificationWorkerShutdownIsNotAnOutboxFailure(t *testing.T) {
	x := newTestAPI(t)
	var logs bytes.Buffer
	x.api.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The installation gate runs right before the claim; cancelling there is a
	// deterministic stand-in for a shutdown racing the claim.
	x.api.enabled = func(context.Context) bool { cancel(); return true }
	done := make(chan struct{})
	go func() {
		x.api.runNotificationChannel(ctx, domain.NotificationWebhook, make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("notification worker did not stop after cancellation")
	}
	if strings.Contains(logs.String(), "claim notification outbox delivery") {
		t.Fatalf("shutdown was logged as an outbox failure: %s", logs.String())
	}
	_, workers := x.api.healthSnapshot(time.Now().UTC())
	for _, worker := range workers {
		if worker.Name == "notification_webhook" && worker.Status == "degraded" {
			t.Fatalf("shutdown marked the worker degraded: %+v", worker)
		}
	}
}

func TestE2EulerSyncReportsDisableDuringLongPoll(t *testing.T) {
	x := newTestAPI(t)
	x.bootstrapAdmin(t)
	_, token := x.enroll(t)
	var enabled atomic.Bool
	enabled.Store(true)
	x.api.enabled = func(context.Context) bool { return enabled.Load() }
	euler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.api.EulerAgentHandler(r.URL.RequestURI()).ServeHTTP(w, r)
	}))
	defer euler.Close()
	go func() {
		time.Sleep(300 * time.Millisecond)
		enabled.Store(false)
	}()
	status, body := request(t, http.DefaultClient, http.MethodGet, euler.URL+"/agent/v1/sync?wait=10s", nil, map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "application_disabled") {
		t.Fatalf("disable during long poll=%d %q", status, body)
	}
}

// Hints saved by older releases expose most of a short key to every project
// reader (sk-123…5678 for an 11-character key). GET and PUT derive the hint
// from the current key with ids.Hint instead of returning the stored column.
func TestE2AISettingsHintIsDerivedFromCurrentKey(t *testing.T) {
	x := newTestAPI(t)
	x.bootstrapAdmin(t)
	const key = "sk-12345678"
	seed := func(encryptedKey string) {
		t.Helper()
		settings := domain.AISettings{Protocol: domain.AIProtocolOpenAIChat, BaseURL: "https://ai.example/v1", Model: "m", APIKeyHint: "legacy-stored-hint", UpdatedAt: time.Now().UTC()}
		if err := x.store.PutAISettings(context.Background(), store.StoredAISettings{Settings: settings, EncryptedAPIKey: encryptedKey}); err != nil {
			t.Fatal(err)
		}
	}
	hintOf := func(body []byte) string {
		t.Helper()
		var out domain.AISettings
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return out.APIKeyHint
	}
	encrypted, err := x.api.vault.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	seed(encrypted)
	settingsURL := x.server.URL + "/api/v1/ai/settings"
	status, body := request(t, x.admin, http.MethodGet, settingsURL, nil, nil)
	if status != http.StatusOK || hintOf(body) != ids.Hint(key) {
		t.Fatalf("GET returned hint from %d %s, want %q", status, body, ids.Hint(key))
	}
	// Saving without re-entering the key keeps the key and replaces the stored hint.
	status, body = request(t, x.admin, http.MethodPut, settingsURL, map[string]any{"protocol": "openai_chat", "baseUrl": "https://ai.example/v1", "model": "m2"}, nil)
	if status != http.StatusOK || hintOf(body) != ids.Hint(key) {
		t.Fatalf("PUT returned hint from %d %s, want %q", status, body, ids.Hint(key))
	}
	stored, err := x.store.AISettings(context.Background())
	if err != nil || stored.Settings.APIKeyHint != ids.Hint(key) || stored.EncryptedAPIKey != encrypted {
		t.Fatalf("stored hint=%q key kept=%v err=%v", stored.Settings.APIKeyHint, stored.EncryptedAPIKey == encrypted, err)
	}
	// A key that no longer decrypts (for example after a root-key change) is
	// still reported as configured, with a neutral hint.
	other, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	seed(foreign)
	status, body = request(t, x.admin, http.MethodGet, settingsURL, nil, nil)
	if status != http.StatusOK || hintOf(body) != "----" || !strings.Contains(string(body), `"keyConfigured":true`) {
		t.Fatalf("undecryptable key: %d %s", status, body)
	}
}

// The platform exposes exactly ManagementRoutes(), while EulerManagementHandler
// keeps its own registration list. A pattern missing there (or registered with
// another method) answers the ServeMux's plain-text 404/405 for a route the
// platform has already authorized.
func TestE2EveryManagementRouteIsServed(t *testing.T) {
	x := newTestAPI(t)
	handler := x.api.EulerManagementHandler("euler_actor")
	placeholders := strings.NewReplacer("{id}", "e2_missing", "{stepId}", "e2_missing_step", "{capability}", "network.auth_bruteforce")
	seen := map[string]bool{}
	for _, route := range ManagementRoutes() {
		method, path, ok := strings.Cut(route.Pattern, " ")
		if !ok || !strings.HasPrefix(path, "/api/v1/") || seen[route.Pattern] {
			t.Fatalf("malformed or duplicate management route %q", route.Pattern)
		}
		seen[route.Pattern] = true
		// WITSHIELD.md: read covers every GET business read; changes need write or manage.
		if (method == http.MethodGet) != (route.Permission == "read") || (route.Permission != "read" && route.Permission != "write" && route.Permission != "manage") {
			t.Errorf("route %q requires %q", route.Pattern, route.Permission)
		}
		var body io.Reader
		if method != http.MethodGet {
			body = strings.NewReader(`{}`)
		}
		r := httptest.NewRequest(method, placeholders.Replace(path), body)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		// Business handlers answer in JSON, including their own 404 for the
		// placeholder IDs; only the mux fallback answers in plain text.
		if (w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed) && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s is not served by EulerManagementHandler: %d %q", route.Pattern, w.Code, w.Body.String())
		}
	}
	if len(seen) != 44 {
		t.Fatalf("management route count %d, want 44", len(seen))
	}
}
