package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/agent"
)

// Every management request of an engine goes through one shared handler, and
// the Euler actor travels with each request. Concurrent requests from
// different actors must each be recorded under their own actor, and a request
// without an actor must not borrow one from a request running beside it.
func TestConcurrentManagementRequestsKeepTheirOwnActor(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, Config{DataDir: t.TempDir(), Key: bytes.Repeat([]byte{7}, 32), Enabled: func(context.Context) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.ServeAgent(w, r, r.RequestURI)
	}))
	defer server.Close()
	w := management(t, e, "POST", "/enrollment-tokens", map[string]any{"name": "actor test", "expiresIn": "15m"})
	if w.Code != 201 {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	var enrollment struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := agent.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	client, err := agent.NewObserverClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	device, _, err := client.Enroll(ctx, agent.EnrollRequest{EnrollmentToken: enrollment.Token, Name: "host", Hostname: "host", OS: "linux", Arch: "amd64", AgentVersion: "euler-test", IdentityPublicKey: pub, IdentityPrivateKey: priv, ScanInterval: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"deviceId": device.ID, "type": "package_security_upgrade", "parameters": map[string]any{"packages": []string{"openssl"}}})
	const requests = 24
	actor := func(i int) string {
		if i%8 == 0 {
			return "" // no Euler actor: must be refused
		}
		return fmt.Sprintf("actor-%02d", i)
	}
	actions := make([]string, requests)
	failures := make(chan string, requests)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range requests {
		done.Add(1)
		go func() {
			defer done.Done()
			r := httptest.NewRequest("POST", "/api/v1/actions", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			start.Wait()
			e.ServeManagement(w, r, actor(i))
			if actor(i) == "" {
				if w.Code != http.StatusUnauthorized {
					failures <- fmt.Sprintf("request %d without actor: %d %s", i, w.Code, w.Body)
				}
				return
			}
			var created struct {
				Action struct {
					ID string `json:"id"`
				} `json:"action"`
			}
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Action.ID == "" {
				failures <- fmt.Sprintf("request %d as %s: %d %s", i, actor(i), w.Code, w.Body)
				return
			}
			actions[i] = created.Action.ID
		}()
	}
	start.Done()
	done.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if t.Failed() {
		return
	}
	for i, id := range actions {
		if actor(i) == "" {
			continue
		}
		w := management(t, e, "GET", "/audit?actionId="+id, nil)
		var audit struct {
			Items []struct {
				Actor string `json:"actor"`
				Event string `json:"event"`
			} `json:"items"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &audit) != nil || len(audit.Items) != 1 {
			t.Fatalf("audit of %s: %d %s", id, w.Code, w.Body)
		}
		if got := audit.Items[0]; got.Event != "preview_created" || got.Actor != actor(i) {
			t.Errorf("action %s created by %s was recorded as %+v", id, actor(i), got)
		}
	}
}
