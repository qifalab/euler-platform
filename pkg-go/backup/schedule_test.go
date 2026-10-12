package backup

import (
	"errors"
	"testing"
	"time"
)

func TestParseScheduleCanonicalForms(t *testing.T) {
	for in, want := range map[string]string{
		"daily 02:30":  "daily 02:30",
		"02:30":        "daily 02:30",
		" Daily 23:59": "daily 23:59",
		"00:00":        "daily 00:00",
		"every 6h":     "every 6h",
		"EVERY 24H":    "every 24h",
		"every 720h":   "every 720h",
		"every 1h":     "every 1h",
	} {
		s, err := ParseSchedule(in)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", in, err)
		}
		if got := s.String(); got != want {
			t.Fatalf("ParseSchedule(%q) = %q, want %q", in, got, want)
		}
		// The canonical form parses back to itself.
		again, err := ParseSchedule(s.String())
		if err != nil || again != s {
			t.Fatalf("canonical %q does not round-trip: %+v %v", s, again, err)
		}
	}
}

func TestParseScheduleRejects(t *testing.T) {
	for _, in := range []string{
		"", "daily", "24:00", "12:60", "2:30", "02:3", "02-30", "daily 2:30",
		"every 0h", "every 721h", "every h", "every 6", "every -1h", "every 06h",
		"every 6m", "0 2 * * *", "weekly 02:30", "daily 02:30 utc",
	} {
		if _, err := ParseSchedule(in); !errors.Is(err, ErrInvalidSchedule) {
			t.Fatalf("ParseSchedule(%q) = %v, want ErrInvalidSchedule", in, err)
		}
	}
}

func TestScheduleNextDaily(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	s, _ := ParseSchedule("daily 02:30")
	// 01:00 CST: today's 02:30 is still ahead.
	last := time.Date(2026, 10, 8, 1, 0, 0, 0, cst)
	if got, want := s.Next(last, cst), time.Date(2026, 10, 8, 2, 30, 0, 0, cst); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	// Exactly 02:30: the run just happened, the next one is tomorrow.
	last = time.Date(2026, 10, 8, 2, 30, 0, 0, cst)
	if got, want := s.Next(last, cst), time.Date(2026, 10, 9, 2, 30, 0, 0, cst); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	// The location matters: 20:00 UTC is 04:00 CST the next day.
	last = time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	if got, want := s.Next(last, cst), time.Date(2026, 10, 10, 2, 30, 0, 0, cst); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	if got, want := s.Next(last, nil), time.Date(2026, 10, 9, 2, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("nil location: next = %v, want %v", got, want)
	}
}

func TestScheduleNextInterval(t *testing.T) {
	s, _ := ParseSchedule("every 6h")
	last := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	if got, want := s.Next(last, nil), last.Add(6*time.Hour); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestRetainUntilMatchesExpiredSnapshots(t *testing.T) {
	taken := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	until := RetainUntil(taken, 7)
	if want := taken.Add(7 * 24 * time.Hour); !until.Equal(want) {
		t.Fatalf("RetainUntil = %v, want %v", until, want)
	}
	snaps := []Snapshot{{SnapshotID: "s", TakenAt: taken}}
	for _, tc := range []struct {
		now     time.Time
		expired bool
	}{
		{until.Add(-time.Second), false},
		{until, false}, // the keep-by instant itself is still retained
		{until.Add(time.Second), true},
	} {
		if got := PastRetention(until, tc.now); got != tc.expired {
			t.Fatalf("PastRetention at %v = %v, want %v", tc.now, got, tc.expired)
		}
		// The stored deadline and the policy sweep agree at every instant.
		if got := len(ExpiredSnapshots(snaps, 7, tc.now)) == 1; got != tc.expired {
			t.Fatalf("ExpiredSnapshots at %v disagrees with PastRetention", tc.now)
		}
	}
	if !RetainUntil(taken, 0).IsZero() || PastRetention(time.Time{}, taken.AddDate(100, 0, 0)) {
		t.Fatal("retention 0 must mean retain forever")
	}
}
