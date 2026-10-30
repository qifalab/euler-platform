package statistics

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

// daily_events rows carry a unique_visitors column; it must hold the real
// per-day distinct event visitors, not a placeholder zero.
func TestCSVDailyEventsReportDistinctVisitors(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	day := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour).Add(12 * time.Hour)
	seedEvent(t, m, s, "alice", "signup", day)
	seedEvent(t, m, s, "alice", "signup", day.Add(time.Minute))
	seedEvent(t, m, s, "bob", "signup", day.Add(2*time.Minute))
	date := day.Format(time.DateOnly)
	w := call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/export.csv?from="+date+"&to="+date+"&timezone=UTC", "", "")
	requireStatus(t, w, 200)
	rows, e := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, row := range rows {
		if row[0] == "daily_events" && row[5] == date {
			found = true
			if row[6] != "3" || row[7] != "2" {
				t.Fatalf("daily_events count=%s unique_visitors=%s, want 3 and 2", row[6], row[7])
			}
		}
	}
	if !found {
		t.Fatalf("no daily_events row for %s: %v", date, rows)
	}
}
