package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/errors"
)

// TestE2ptrNonErrorsxDoesNotPanic pins the comma-ok narrowing: a plain error
// (not *errorsx.Error) must be wrapped as an internal error, never panic.
func TestE2ptrNonErrorsxDoesNotPanic(t *testing.T) {
	e := e2ptr(errors.New("plain"))
	if e == nil || e.Code != "Common.InternalError" {
		t.Fatalf("expected wrapped internal error, got %+v", e)
	}
	// A local failure's text (e.g. a JSON decode error describing a downstream
	// payload) must not reach the client.
	var target []struct{ N int }
	decodeErr := json.Unmarshal([]byte(`{"Data":1}`), &target)
	if got := e2ptr(decodeErr); strings.Contains(got.Message, "json") || strings.Contains(got.Message, "unmarshal") {
		t.Fatalf("raw decode error echoed to the client: %q", got.Message)
	}

	orig := errorsx.New("Some.Code", errorsx.StatusBadRequest, "msg")
	if got := e2ptr(orig); got != orig {
		t.Fatalf("errorsx error should pass through unchanged")
	}

	if e := e2ptr(nil); e == nil || e.Code != "Common.InternalError" {
		t.Fatalf("nil error should still produce an internal error, got %+v", e)
	}
}

// Regression: periods were computed as now.AddDate(0, -i, 0); on Oct 31 that
// normalises "Sep 31" to Oct 1, so October was listed twice and September never.
func TestBillPeriodsAtMonthEnd(t *testing.T) {
	cases := map[string][]string{
		"2026-10-31T23:30:00Z": {"2026-10", "2026-09", "2026-08", "2026-07", "2026-06", "2026-05"},
		"2026-03-31T00:00:00Z": {"2026-03", "2026-02", "2026-01", "2025-12", "2025-11", "2025-10"},
		"2028-02-29T12:00:00Z": {"2028-02", "2028-01", "2027-12", "2027-11", "2027-10", "2027-09"},
	}
	for at, want := range cases {
		now, _ := time.Parse(time.RFC3339, at)
		if got := billPeriods(now, 6); !reflect.DeepEqual(got, want) {
			t.Errorf("billPeriods(%s) = %v, want %v", at, got, want)
		}
	}
}

func TestHandleBillsQueriesSixDistinctMonthsOnOct31(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("period"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"BillPeriod": r.URL.Query().Get("period"), "TotalAmount": "0.00"})
	}))
	defer billing.Close()

	s := newConsoleStore()
	s.billingURL = billing.URL
	s.now = func() time.Time { return time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC) }

	req := httptest.NewRequest(http.MethodGet, "/console/bills", nil)
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleBills(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []struct {
			BillPeriod string `json:"BillPeriod"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range env.Data {
		got = append(got, b.BillPeriod)
	}
	want := []string{"2026-10", "2026-09", "2026-08", "2026-07", "2026-06", "2026-05"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bill periods = %v, want %v (asked billing for %v)", got, want, asked)
	}
}

// Regression: ResourceCount.Total was len() of every ledger row, released and
// failed creates included.
func TestOverviewResourceCountExcludesReleasedAndFailed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/balance", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Available": "1.00", "Frozen": "0.00", "Total": "1.00"})
	})
	mux.HandleFunc("/api/v1/orchestrator/resources", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": []map[string]string{
			{"resourceId": "r1", "state": "RUNNING"},
			{"resourceId": "r2", "state": "STOPPED"},
			{"resourceId": "r3", "state": "RELEASED"},
			{"resourceId": "r4", "state": "CREATE_FAILED"},
			{"resourceId": "r5", "state": "CREATING"},
		}})
	})
	mux.HandleFunc("/api/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": []map[string]string{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := newConsoleStore()
	s.billingURL, s.orchestratorURL, s.orderURL = srv.URL, srv.URL, srv.URL
	req := httptest.NewRequest(http.MethodGet, "/console/overview", nil)
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleOverview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			ResourceCount struct {
				Total int `json:"Total"`
			} `json:"ResourceCount"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ResourceCount.Total != 3 {
		t.Fatalf("ResourceCount.Total = %d, want 3 (RUNNING, STOPPED, CREATING)", env.Data.ResourceCount.Total)
	}
}
