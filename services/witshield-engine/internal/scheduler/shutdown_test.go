// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

type cancelledStore struct{}

func (cancelledStore) DueSchedules(ctx context.Context, _ time.Time, _ int) ([]domain.Schedule, error) {
	return nil, ctx.Err()
}
func (cancelledStore) MarkScheduleRun(context.Context, string, time.Time, time.Time, time.Time) error {
	return nil
}
func (cancelledStore) EnqueueCommand(context.Context, domain.DeviceCommand) error { return nil }

func TestRunDoesNotReportShutdownAsTickFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs bytes.Buffer
	s := New(cancelledStore{}, slog.New(slog.NewTextHandler(&logs, nil)))
	var observed []error
	s.SetObserver(func(err error) { observed = append(observed, err) })
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	for _, err := range observed {
		if err != nil {
			t.Fatalf("observer received %v for a cancelled tick", err)
		}
	}
	if strings.Contains(logs.String(), "schedule tick failed") {
		t.Fatalf("shutdown logged as a tick failure: %s", logs.String())
	}
}
