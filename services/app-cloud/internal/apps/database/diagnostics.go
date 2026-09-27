package database

import (
	"context"
	"sync"
)

// DependencyStatus probes the existing engine connections without provisioning
// resources. Only fixed states leave this boundary, never DSNs or driver errors.
func (m *Module) DependencyStatus(ctx context.Context) map[string]string {
	out := map[string]string{"mysql": "not_configured", "postgresql": "not_configured"}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, config := range m.engines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state := "configured_unverified"
			if engine, ok := config.Engine.(*sqlEngine); ok {
				state = "up"
				if err := engine.db.PingContext(ctx); err != nil {
					state = "down"
				}
			}
			mu.Lock()
			out[name] = state
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
