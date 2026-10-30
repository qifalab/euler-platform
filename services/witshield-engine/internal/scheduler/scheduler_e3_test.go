// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/store"
)

type markCall struct {
	id                  string
	expected, ran, next time.Time
}

type recordingStore struct {
	due        []domain.Schedule
	commands   []domain.DeviceCommand
	marks      []markCall
	enqueueErr map[string]error
	markErr    error
}

func (r *recordingStore) DueSchedules(context.Context, time.Time, int) ([]domain.Schedule, error) {
	return r.due, nil
}
func (r *recordingStore) MarkScheduleRun(_ context.Context, id string, expected, ran, next time.Time) error {
	r.marks = append(r.marks, markCall{id: id, expected: expected, ran: ran, next: next})
	return r.markErr
}
func (r *recordingStore) EnqueueCommand(_ context.Context, c domain.DeviceCommand) error {
	if err := r.enqueueErr[c.DeviceID]; err != nil {
		return err
	}
	r.commands = append(r.commands, c)
	return nil
}

// The store advances a schedule only when next_run_at still equals the value
// the scheduler read (optimistic concurrency), and missed runs must collapse to
// one scan while keeping the configured phase instead of drifting to "now".
func TestTickAdvancesWithReadValueAndKeepsPhase(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	readNext := now.Add(-150 * time.Minute) // 2.5 intervals missed
	r := &recordingStore{due: []domain.Schedule{{ID: "s", DeviceID: "d", Kind: domain.ScheduleScan, Interval: time.Hour, NextRunAt: readNext}}}
	s := New(r, nil)
	s.now = func() time.Time { return now }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.commands) != 1 {
		t.Fatalf("missed runs were not collapsed into one scan: %#v", r.commands)
	}
	if len(r.marks) != 1 {
		t.Fatalf("schedule was not advanced: %#v", r.marks)
	}
	got := r.marks[0]
	if got.id != "s" || !got.expected.Equal(readNext) || !got.ran.Equal(now) || !got.next.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("mark=%+v, want expected=%s ran=%s next=%s", got, readNext, now, now.Add(30*time.Minute))
	}
}

// A schedule exactly due now runs now and next fires one full interval later.
func TestTickScheduleDueExactlyNow(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &recordingStore{due: []domain.Schedule{{ID: "s", DeviceID: "d", Kind: domain.ScheduleScan, Interval: 15 * time.Minute, NextRunAt: now}}}
	s := New(r, nil)
	s.now = func() time.Time { return now }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.marks) != 1 || !r.marks[0].next.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("marks=%+v", r.marks)
	}
}

// A failed enqueue must leave the schedule due (retried next tick) without
// blocking the other schedules; a concurrent update (ErrConflict) is not an error.
func TestTickEnqueueFailureKeepsScheduleDueAndConflictIsBenign(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &recordingStore{
		due: []domain.Schedule{
			{ID: "broken", DeviceID: "d1", Kind: domain.ScheduleScan, Interval: time.Hour, NextRunAt: now.Add(-time.Minute)},
			{ID: "ok", DeviceID: "d2", Kind: domain.ScheduleScan, Interval: time.Hour, NextRunAt: now.Add(-time.Minute)},
		},
		enqueueErr: map[string]error{"d1": errors.New("database is locked")},
		markErr:    store.ErrConflict,
	}
	s := New(r, nil)
	s.now = func() time.Time { return now }
	err := s.Tick(context.Background())
	if err == nil || errors.Is(err, store.ErrConflict) {
		t.Fatalf("tick error=%v, want only the enqueue failure", err)
	}
	if len(r.commands) != 1 || r.commands[0].DeviceID != "d2" {
		t.Fatalf("other schedules were blocked: %#v", r.commands)
	}
	if len(r.marks) != 1 || r.marks[0].id != "ok" {
		t.Fatalf("failed schedule was advanced without a queued scan: %+v", r.marks)
	}
}
