package statistics

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func seedView(t *testing.T, m *Module, s site, visitor string, at time.Time) {
	t.Helper()
	_, e := m.rt.DB.Exec("INSERT INTO statistics_views(site_id,visitor_hash,ip_hash,url,referrer,title,resolution,language,user_agent,visited_at) VALUES(?,?,?,?,?,?,?,?,?,?)", s.ID, visitor, "ip", "https://docs.example/a", "https://search.example/path", "title", "", "", "Mozilla/5.0 (iPhone; Mobile) Safari/605.1", at.UTC().Format(storedTimeLayout))
	if e != nil {
		t.Fatal(e)
	}
}
func seedEvent(t *testing.T, m *Module, s site, visitor, name string, at time.Time) {
	t.Helper()
	_, e := m.rt.DB.Exec("INSERT INTO statistics_events(site_id,visitor_hash,ip_hash,name,url,visited_at) VALUES(?,?,?,?,?,?)", s.ID, visitor, "ip", name, "https://docs.example/a", at.UTC().Format(storedTimeLayout))
	if e != nil {
		t.Fatal(e)
	}
}

func TestAnalyticsDateTimezoneDeduplicationAndScope(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	other := createSite(t, m, scope)
	day := time.Now().UTC().AddDate(0, 0, -2)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	seedView(t, m, s, "alice", start.Add(-time.Nanosecond))
	seedView(t, m, s, "alice", start)
	seedView(t, m, s, "alice", start.Add(2*time.Hour))
	seedView(t, m, s, "bob", start.Add(24*time.Hour-time.Nanosecond))
	seedView(t, m, s, "alice", start.Add(24*time.Hour))
	seedView(t, m, other, "stranger", start)
	q := "?from=" + start.Format(time.DateOnly) + "&to=" + start.Format(time.DateOnly) + "&timezone=Asia%2FShanghai"
	w := call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/analytics"+q, "", "")
	requireStatus(t, w, 200)
	var out analysis
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	if out.PageViews != 3 || out.UniqueVisitors != 2 || len(out.Trend) != 1 || out.Trend[0].PageViews != 3 || out.Period.Start != start.UTC().Format(storedTimeLayout) {
		t.Fatalf("wrong range/dedup %s", w.Body.String())
	}
	if out.Sources[0].Name != "search.example" || out.Devices[0].Name != "手机" || out.Browsers[0].Name != "Safari" {
		t.Fatalf("wrong dimensions: %+v", out)
	}
	// A visitor across two days remains one visitor in the selected range.
	q = "?from=" + start.Format(time.DateOnly) + "&to=" + start.AddDate(0, 0, 1).Format(time.DateOnly) + "&timezone=Asia%2FShanghai"
	w = call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/analytics"+q, "", "")
	requireStatus(t, w, 200)
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.PageViews != 4 || out.UniqueVisitors != 2 || out.Trend[1].UniqueVisitors != 1 {
		t.Fatalf("cross-day UV summed: %s", w.Body.String())
	}
	w = call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/report"+q, "", "")
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"pageViews":4`) || !strings.Contains(w.Body.String(), `"uniqueVisitors":2`) {
		t.Fatal(w.Body.String())
	}
	wrong := scope
	wrong.ProjectID = "other"
	requireStatus(t, call(t, m.Handler(), &wrong, "GET", "/sites/"+s.ID+"/analytics"+q, "", ""), 404)
	requireStatus(t, call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/analytics?timezone=Invalid%2FZone", "", ""), 400)
	requireStatus(t, call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/analytics?from=2026-01-03&to=2026-01-01", "", ""), 400)
}
func TestDSTDayHas23Hours(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	year := time.Now().Year()
	first := time.Date(year, time.March, 1, 0, 0, 0, 0, time.UTC)
	secondSunday := 8 + (7-int(first.Weekday()))%7
	date := fmt.Sprintf("%04d-03-%02d", year, secondSunday)
	w := call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/analytics?from="+date+"&to="+date+"&timezone=America%2FNew_York", "", "")
	requireStatus(t, w, 200)
	var out analysis
	json.Unmarshal(w.Body.Bytes(), &out)
	start, _ := time.Parse(time.RFC3339Nano, out.Period.Start)
	end, _ := time.Parse(time.RFC3339Nano, out.Period.End)
	if end.Sub(start) != 23*time.Hour {
		t.Fatalf("DST boundary: %+v", out.Period)
	}
}
func TestEventRegistrationBoundariesSharedRateAndDisable(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	p := "/sites/" + s.ID
	public := "/sites/" + s.PublicID
	body := `{"visitor_id":"visitor-001","url":"https://docs.example/a?email=private","name":"signup"}`
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", body, "https://docs.example"), 400)
	requireStatus(t, call(t, m.Handler(), &scope, "PUT", p+"/settings", `{"retentionDays":30,"eventNames":["signup","purchase"]}`, ""), 200)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", body, "https://evil.example"), 403)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", strings.TrimSuffix(body, "}")+`,"properties":{"email":"secret@example.test"}}`, "https://docs.example"), 400)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", strings.Replace(body, "signup", "unregistered", 1), "https://docs.example"), 400)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", body, "https://docs.example"), 202)
	var savedURL, visitor string
	m.rt.DB.QueryRow("SELECT url,visitor_hash FROM statistics_events").Scan(&savedURL, &visitor)
	if strings.Contains(savedURL, "private") || visitor == "visitor-001" {
		t.Fatal("raw query/identifier persisted")
	}
	for i := 0; i < 119; i++ {
		requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/collect", `{"visitor_id":"visitor-001","url":"https://docs.example/a"}`, "https://docs.example"), 202)
	}
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", body, "https://docs.example"), 429)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/collect", `{"visitor_id":"visitor-001","url":"https://docs.example/a"}`, "https://docs.example"), 429)
	m.rt.DB.Exec("UPDATE statistics_sites SET enabled=0 WHERE id=?", s.ID)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", body, "https://docs.example"), 404)
	m.rt.DB.Exec("UPDATE statistics_sites SET enabled=1 WHERE id=?", s.ID)
	m.rt.DB.Exec("UPDATE installations SET status='disabled'")
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", public+"/events", body, "https://docs.example"), 404)
}
func TestFunnelSameVisitorOrderWindowRepeatedStartsAndSite(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	other := createSite(t, m, scope)
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	// Alice finishes. Bob has reversed order. Carol exceeds the window.
	for _, e := range []struct {
		visitor, name string
		minute        int
	}{{"alice", "start", 0}, {"alice", "middle", 1}, {"alice", "finish", 2}, {"alice", "finish", 3}, {"bob", "finish", 0}, {"bob", "middle", 1}, {"bob", "start", 2}, {"carol", "start", 0}, {"carol", "middle", 20}, {"carol", "finish", 21}, {"dave", "start", 0}, {"dave", "start", 20}, {"dave", "middle", 21}, {"dave", "finish", 22}, {"eve", "start", 0}, {"frank", "middle", 1}, {"frank", "finish", 2}} {
		seedEvent(t, m, s, e.visitor, e.name, base.Add(time.Duration(e.minute)*time.Minute))
	}
	seedEvent(t, m, other, "eve", "middle", base.Add(time.Minute))
	seedEvent(t, m, other, "eve", "finish", base.Add(2*time.Minute))
	q := "?from=" + base.Format(time.DateOnly) + "&to=" + base.Format(time.DateOnly)
	w := call(t, m.Handler(), &scope, "POST", "/sites/"+s.ID+"/funnel"+q, `{"steps":["start","middle","finish"],"windowMinutes":10}`, "")
	requireStatus(t, w, 200)
	var out struct{ Steps []funnelStep }
	json.Unmarshal(w.Body.Bytes(), &out)
	for i, want := range []int{5, 2, 2} {
		if out.Steps[i].Visitors != want {
			t.Fatalf("step %d wrong: %s", i, w.Body.String())
		}
	}
	// Repeated stage name requires a second distinct event.
	w = call(t, m.Handler(), &scope, "POST", "/sites/"+s.ID+"/funnel"+q, `{"steps":["start","start"],"windowMinutes":60}`, "")
	requireStatus(t, w, 200)
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Steps[1].Visitors != 1 {
		t.Fatal(w.Body.String())
	}
}
func TestRetentionCleanupCSVInjectionAndPermissions(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	other := createSite(t, m, scope)
	old := time.Now().UTC().AddDate(0, 0, -40)
	seedView(t, m, s, "old", old)
	seedEvent(t, m, s, "old", "signup", old)
	seedView(t, m, other, "old", old)
	seedView(t, m, s, "fresh", time.Now().UTC())
	viewer := scope
	viewer.Permissions = []string{"read"}
	path := "/sites/" + s.ID
	requireStatus(t, call(t, m.Handler(), &viewer, "PUT", path+"/settings", `{"retentionDays":7,"eventNames":[]}`, ""), 403)
	requireStatus(t, call(t, m.Handler(), &scope, "PUT", path+"/settings", `{"retentionDays":7,"eventNames":[]}`, ""), 200)
	var n int
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM statistics_views WHERE site_id=?", s.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("retention: %d", n)
	}
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM statistics_events WHERE site_id=?", s.ID).Scan(&n)
	if n != 0 {
		t.Fatal("expired events remain")
	}
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM statistics_views WHERE site_id=?", other.ID).Scan(&n)
	if n != 1 {
		t.Fatal("cleanup leaked across site")
	}
	m.rt.DB.Exec("UPDATE statistics_sites SET name=? WHERE id=?", " =HYPERLINK(\"evil\")", s.ID)
	w := call(t, m.Handler(), &scope, "GET", path+"/export.csv?timezone="+url.QueryEscape("Asia/Shanghai"), "", "")
	requireStatus(t, w, 200)
	rows, e := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) < 2 || !strings.HasPrefix(rows[1][1], "' =") {
		t.Fatalf("formula not escaped: %s", w.Body.String())
	}
	for _, value := range []string{"=1+1", " +cmd", "\t@SUM(1)", "\r-1", "\uFEFF=1"} {
		if !strings.HasPrefix(csvSafe(value), "'") {
			t.Errorf("not safe %q", value)
		}
	}
	requireStatus(t, call(t, m.Handler(), &viewer, "POST", path+"/cleanup", "", ""), 403)
	requireStatus(t, call(t, m.Handler(), &scope, "PUT", path+"/settings", `{"retentionDays":0,"eventNames":[]}`, ""), 400)
	requireStatus(t, call(t, m.Handler(), &scope, "PUT", path+"/settings", `{"retentionDays":30,"eventNames":["signup","signup"]}`, ""), 400)
}

func TestMaintenancePrunesPausedSitesWithoutTraffic(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	old := time.Now().UTC().AddDate(0, 0, -366)
	seedView(t, m, s, "old", old)
	seedEvent(t, m, s, "old", "signup", old)
	if _, e := m.rt.DB.Exec("UPDATE statistics_sites SET enabled=0 WHERE id=?", s.ID); e != nil {
		t.Fatal(e)
	}
	if e := m.Maintain(context.Background()); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := m.rt.DB.QueryRow("SELECT (SELECT COUNT(*) FROM statistics_views)+(SELECT COUNT(*) FROM statistics_events)").Scan(&n); e != nil || n != 0 {
		t.Fatalf("idle maintenance: %d %v", n, e)
	}
}

func TestLegacyTimestampNormalizationPreservesNanoseconds(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	if _, e := m.rt.DB.Exec("DELETE FROM statistics_migrations"); e != nil {
		t.Fatal(e)
	}
	for _, at := range []string{"2026-09-01T00:00:00Z", "2026-09-01T00:00:00.1Z", "2026-09-01T00:00:00.000000001Z"} {
		if _, e := m.rt.DB.Exec("INSERT INTO statistics_events(site_id,visitor_hash,ip_hash,name,url,visited_at) VALUES(?,?,?,?,?,?)", s.ID, "v", "ip", "signup", "https://docs.example/", at); e != nil {
			t.Fatal(e)
		}
	}
	if e := m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	rows, e := m.rt.DB.Query("SELECT visited_at FROM statistics_events ORDER BY visited_at")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	expected := []string{"2026-09-01T00:00:00.000000000Z", "2026-09-01T00:00:00.000000001Z", "2026-09-01T00:00:00.100000000Z"}
	i := 0
	for rows.Next() {
		var at string
		if e = rows.Scan(&at); e != nil {
			t.Fatal(e)
		}
		if i >= len(expected) || at != expected[i] {
			t.Fatalf("normalization at %d: %q", i, at)
		}
		i++
	}
	if e = rows.Err(); e != nil || i != len(expected) {
		t.Fatalf("count=%d error=%v", i, e)
	}
}
