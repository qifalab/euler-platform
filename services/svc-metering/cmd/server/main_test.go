package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func ingest(t *testing.T, s *meteringStore, acct int64, resourceID, metric, value string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"resourceId":  resourceID,
		"productCode": "scecs",
		"metric":      metric,
		"value":       value,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/metering/ingest", bytes.NewReader(body))
	req.Header.Set(accountIDHeader, fmt.Sprintf("%d", acct))
	rr := httptest.NewRecorder()
	s.handleIngest(rr, req)
	return rr
}

// TestIngestConcurrent exercises concurrent ingest across resources; run with
// -race to verify s.regions/s.types/s.records are only touched under s.mu.
func TestIngestConcurrent(t *testing.T) {
	s := newMeteringStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := fmt.Sprintf("res-%d", i)
			for j := 0; j < 50; j++ {
				rr := ingest(t, s, int64(1000+i), res, "cpu_core_hour", "0.033333")
				if rr.Code != http.StatusOK {
					t.Errorf("ingest %s: %d %s", res, rr.Code, rr.Body.String())
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestIngestOwnership verifies a foreign account cannot write usage for an
// already-owned resource.
func TestIngestOwnership(t *testing.T) {
	s := newMeteringStore()
	if rr := ingest(t, s, 1, "res-own", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
		t.Fatalf("first ingest: %d", rr.Code)
	}
	if rr := ingest(t, s, 2, "res-own", "cpu_core_hour", "1"); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign ingest: got %d, want 403", rr.Code)
	}
}

// TestIngestBodyLimit verifies oversized bodies are rejected.
func TestIngestBodyLimit(t *testing.T) {
	s := newMeteringStore()
	big := bytes.Repeat([]byte("a"), maxBodyBytes+16)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/metering/ingest", bytes.NewReader(big))
	req.Header.Set(accountIDHeader, "1")
	rr := httptest.NewRecorder()
	s.handleIngest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 400", rr.Code)
	}
}
