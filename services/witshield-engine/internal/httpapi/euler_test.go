// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package httpapi

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestEulerDisableCancelsInFlightExternalWork(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	s := &Server{enabled: func(context.Context) bool { return enabled.Load() }}
	ctx, cancel := s.operationContext(context.Background())
	defer cancel()
	enabled.Store(false)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("disabled installation kept operation alive")
	}
}
func TestEulerBusinessRoutePermissions(t *testing.T) {
	routes := ManagementRoutes()
	if len(routes) != 44 {
		t.Fatalf("business route count %d", len(routes))
	}
	for _, r := range routes {
		if r.Pattern == "POST /api/v1/actions/{id}/approve" && r.Permission != "manage" {
			t.Fatal("approval must need manage")
		}
		if r.Pattern == "GET /api/v1/schedules" && r.Permission != "read" {
			t.Fatal("schedule read requires extra privilege")
		}
	}
}
