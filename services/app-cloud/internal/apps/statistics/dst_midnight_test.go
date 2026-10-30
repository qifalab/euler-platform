package statistics

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

// skippedMidnight finds a recent calendar day, inside the default 365-day
// retention, on which local midnight does not exist in loc because DST starts
// at 00:00. Returned as a UTC calendar date.
func skippedMidnight(loc *time.Location) (time.Time, bool) {
	today := time.Now().UTC()
	for back := 5; back < 330; back++ {
		day := time.Date(today.Year(), today.Month(), today.Day()-back, 0, 0, 0, 0, time.UTC)
		local := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
		if local.Hour() != 0 || local.Day() != day.Day() {
			return day, true
		}
	}
	return time.Time{}, false
}

// Santiago and Cairo move clocks from 00:00 to 01:00. Go normalizes the
// missing midnight backwards (Santiago) or forwards (Cairo); either way every
// selected local day must appear exactly once and own exactly its own hours.
func TestTrendKeepsEveryLocalDayWhenDSTSkipsMidnight(t *testing.T) {
	for _, zone := range []string{"America/Santiago", "Africa/Cairo"} {
		t.Run(zone, func(t *testing.T) {
			loc, e := time.LoadLocation(zone)
			if e != nil {
				t.Fatal(e)
			}
			gap, ok := skippedMidnight(loc)
			if !ok {
				t.Skipf("no DST change at local midnight in %s within the retention window", zone)
			}
			m, scope := fixture(t)
			s := createSite(t, m, scope)
			days := []time.Time{gap.AddDate(0, 0, -2), gap.AddDate(0, 0, -1), gap, gap.AddDate(0, 0, 1), gap.AddDate(0, 0, 2)}
			for _, d := range days {
				seedView(t, m, s, "noon", time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc))
			}
			// The last half hour before the skipped midnight belongs to the previous day.
			eve := days[1]
			seedView(t, m, s, "late", time.Date(eve.Year(), eve.Month(), eve.Day(), 23, 30, 0, 0, loc))
			get := func(from, to time.Time) analysis {
				t.Helper()
				q := "?from=" + from.Format(time.DateOnly) + "&to=" + to.Format(time.DateOnly) + "&timezone=" + url.QueryEscape(zone)
				w := call(t, m.Handler(), &scope, "GET", "/sites/"+s.ID+"/analytics"+q, "", "")
				requireStatus(t, w, 200)
				var out analysis
				if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
					t.Fatal(e)
				}
				return out
			}
			out := get(days[0], days[4])
			if len(out.Trend) != len(days) {
				t.Fatalf("trend has %d days, want %d: %+v", len(out.Trend), len(days), out.Trend)
			}
			for i, d := range days {
				want := 1
				if i == 1 {
					want = 2
				}
				if out.Trend[i].Date != d.Format(time.DateOnly) || out.Trend[i].PageViews != want {
					t.Fatalf("trend[%d]=%+v, want %s with %d views; trend %+v", i, out.Trend[i], d.Format(time.DateOnly), want, out.Trend)
				}
			}
			if out.PageViews != 6 {
				t.Fatalf("range views=%d, want 6", out.PageViews)
			}
			if out = get(eve, eve); out.PageViews != 2 {
				t.Fatalf("day before the DST change counted %d views, want 2; period %+v", out.PageViews, out.Period)
			}
			out = get(gap, gap)
			if out.PageViews != 1 {
				t.Fatalf("DST change day counted %d views, want 1; period %+v", out.PageViews, out.Period)
			}
			start, e := time.Parse(time.RFC3339Nano, out.Period.Start)
			if e != nil {
				t.Fatal(e)
			}
			if start.In(loc).Format(time.DateOnly) != gap.Format(time.DateOnly) || start.Add(-time.Nanosecond).In(loc).Format(time.DateOnly) != eve.Format(time.DateOnly) {
				t.Fatalf("DST change day starts at %s", start.In(loc))
			}
		})
	}
}
