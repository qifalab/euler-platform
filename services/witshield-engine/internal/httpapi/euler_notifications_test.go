// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEulerNotificationCredentialsCannotFollowChangedDestination(t *testing.T) {
	x := newTestAPI(t)
	initial := map[string]any{"webhookEnabled": true, "webhookUrl": "https://notify.example.test/hooks/a", "webhookSecret": "webhook-secret-at-least-16", "smtpEnabled": true, "smtpHost": "smtp.example.test", "smtpPort": 587, "smtpUsername": "account-a", "smtpPassword": "original-smtp-password", "smtpFrom": "sender@example.test", "smtpTo": []string{"recipient@example.test"}}
	call := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("PUT", "/api/v1/notifications/settings", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		x.api.EulerManagementHandler("euler_actor").ServeHTTP(w, r)
		return w
	}
	if w := call(initial); w.Code != 200 {
		t.Fatalf("initial %d %s", w.Code, w.Body)
	}
	changes := []struct {
		field string
		value any
	}{{"webhookUrl", "https://other.example.test/hooks/a"}, {"webhookUrl", "https://notify.example.test/hooks/other"}, {"webhookUrl", "http://notify.example.test/hooks/a"}, {"smtpHost", "other.example.test"}, {"smtpPort", 465}, {"smtpUsername", "account-b"}}
	for _, change := range changes {
		t.Run(change.field+":"+strings.ReplaceAll(fmt.Sprint(change.value), "/", "_"), func(t *testing.T) {
			input := map[string]any{}
			for k, v := range initial {
				input[k] = v
			}
			delete(input, "webhookSecret")
			delete(input, "smtpPassword")
			input[change.field] = change.value
			w := call(input)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "endpoint_credentials_required") {
				t.Fatalf("credential rebound %d %s", w.Code, w.Body)
			}
			current, err := x.store.NotificationSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if current.Settings.WebhookURL != initial["webhookUrl"] || current.Settings.SMTPHost != initial["smtpHost"] || current.Settings.SMTPPort != 587 || current.Settings.SMTPUsername != "account-a" {
				t.Fatal("rejected update mutated stored configuration")
			}
		})
	}
	// Explicit re-entry authorizes the new destination, even when a channel is
	// disabled. Omitting a secret while retaining its destination preserves it.
	initial["smtpHost"] = "new.example.test"
	initial["webhookUrl"] = "https://new.example.test/hook"
	if w := call(initial); w.Code != 200 {
		t.Fatalf("explicit credentials %d %s", w.Code, w.Body)
	}
	delete(initial, "smtpPassword")
	delete(initial, "webhookSecret")
	if w := call(initial); w.Code != 200 {
		t.Fatalf("same destination preserve %d %s", w.Code, w.Body)
	}
	initial["webhookUrl"] = ""
	initial["webhookEnabled"] = false
	initial["clearWebhookSecret"] = true
	initial["smtpHost"] = ""
	initial["smtpEnabled"] = false
	initial["clearSmtpPassword"] = true
	if w := call(initial); w.Code != 200 {
		t.Fatalf("explicit clear %d %s", w.Code, w.Body)
	}
	current, err := x.store.NotificationSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.EncryptedWebhookSecret != "" || current.EncryptedSMTPPassword != "" {
		t.Fatal("clear retained secret")
	}
}
