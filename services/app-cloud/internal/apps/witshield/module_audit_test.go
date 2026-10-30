package witshield

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// A browser that times out, navigates away or switches project cancels the
// request context while the engine is still working. The platform must still
// record the operation result; otherwise the audit trail ends at an
// "operation.requested" row with no outcome for an operation that ran.
func TestCompletionAuditSurvivesClientDisconnect(t *testing.T) {
	m, db, a := fixture(t)
	started := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the body so the server notices the client hanging up.
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer upstream.Close()
	defer upstream.CloseClientConnections()
	settings := map[string]any{"protocol": "openai_chat", "baseUrl": upstream.URL + "/v1", "model": "audit-model", "apiKey": "audit-test-key"}
	if w := invoke(m, &a, "PUT", "/ai/settings", settings); w.Code != 200 {
		t.Fatalf("ai settings %d %s", w.Code, w.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(30 * time.Second):
		}
	}()
	raw, _ := json.Marshal(map[string]any{"message": "status?"})
	r := httptest.NewRequest("POST", "/ai/chat", bytes.NewReader(raw)).WithContext(appkit.WithScope(ctx, a))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	select {
	case <-started:
	default:
		t.Fatalf("the engine never reached the AI upstream: %d %s", w.Code, w.Body)
	}
	rows, err := db.Query(`SELECT summary FROM audit WHERE action='witshield.operation.completed' AND summary LIKE '%/api/v1/ai/chat%'`)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []string
	for rows.Next() {
		var summary string
		if err = rows.Scan(&summary); err != nil {
			t.Fatal(err)
		}
		summaries = append(summaries, summary)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("completion audit after client disconnect: %q (response %d %s)", summaries, w.Code, w.Body)
	}
	if want := fmt.Sprintf("HTTP %d", w.Code); !strings.HasSuffix(summaries[0], want) {
		t.Fatalf("audited %q but responded %d", summaries[0], w.Code)
	}
}
