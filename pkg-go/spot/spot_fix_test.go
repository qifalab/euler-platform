package spot

import (
	"errors"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

// A negative spot price is a market error, never a subsidy: Publish must
// reject it before it enters the append-only price log.
func TestPublishRejectsNegativePrice(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(func() time.Time { return now })
	if _, err := eng.Publish("scecs", "cn-north-1", pricing.MustParseAmount("-0.01")); !errors.Is(err, ErrNegativePrice) {
		t.Fatalf("err = %v, want ErrNegativePrice", err)
	}
	// The rejected price must not have entered the log.
	if _, err := eng.PriceAt("scecs", "cn-north-1", now); !errors.Is(err, ErrNoPriceHistory) {
		t.Fatalf("rejected price leaked into the log: %v", err)
	}
	// Zero is a deliberate floor and remains publishable.
	if _, err := eng.Publish("scecs", "cn-north-1", 0); err != nil {
		t.Fatalf("zero price should publish: %v", err)
	}
}
