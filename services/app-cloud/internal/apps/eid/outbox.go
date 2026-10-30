package eid

import (
	"context"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

const (
	// webhookCallTimeout bounds one bot call, like the HTTP client's timeout.
	webhookCallTimeout = 10 * time.Second
	// webhookTurn is how long one installation may claim new rows before the
	// worker serves the other installations.
	webhookTurn = 25 * time.Second
	// webhookBatch is how many queued rows one turn reads.
	webhookBatch = 25
	// webhookCloseGrace is how long Close lets a delivery in flight finish.
	webhookCloseGrace = 5 * time.Second
)

// outbox hands committed bot notifications to one module-owned worker, so an
// EID response never waits for a bot endpoint.
type outbox struct {
	mu      sync.Mutex
	pending map[string]appkit.Scope // installations with rows to send
	wake    chan struct{}
	grace   time.Duration
	once    sync.Once
	stop    context.CancelFunc // no further rows are claimed
	abort   context.CancelFunc // the delivery in flight is cancelled
	done    chan struct{}
}

// notifyWebhooks asks the worker to send one installation's queued rows. It
// never blocks: requests for the same installation coalesce until the worker
// takes them.
func (m *Module) notifyWebhooks(s appkit.Scope) {
	o := &m.outbox
	o.mu.Lock()
	if o.pending == nil {
		o.pending = map[string]appkit.Scope{}
	}
	o.pending[s.TenantID+"\x00"+s.ProjectID+"\x00"+s.InstallationID] = s
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (m *Module) takeWebhookRequests() []appkit.Scope {
	o := &m.outbox
	o.mu.Lock()
	defer o.mu.Unlock()
	scopes := make([]appkit.Scope, 0, len(o.pending))
	for _, s := range o.pending {
		scopes = append(scopes, s)
	}
	o.pending = nil
	return scopes
}

// startWebhookWorker starts the delivery worker once, like the database and
// storage schedulers. It first sweeps rows left queued by an earlier process,
// then serves notifyWebhooks until Close.
func (m *Module) startWebhookWorker(parent context.Context) {
	o := &m.outbox
	o.once.Do(func() {
		stop, cancelStop := context.WithCancel(parent)
		deliver, cancelDeliver := context.WithCancel(context.WithoutCancel(parent))
		o.stop, o.abort, o.done = cancelStop, cancelDeliver, make(chan struct{})
		go func() {
			defer close(o.done)
			defer cancelDeliver()
			m.sweepWebhooks(stop)
			for {
				select {
				case <-stop.Done():
					return
				case <-o.wake:
				}
				for _, s := range m.takeWebhookRequests() {
					if m.flushWebhooks(stop, deliver, s) {
						m.notifyWebhooks(s) // rows remain: continue after the others
					}
				}
			}
		}()
	})
}

// sweepWebhooks requests delivery for every installation that still has
// queued rows, such as rows committed just before a restart. Like other
// background work it skips projects whose EID app is disabled or archived.
func (m *Module) sweepWebhooks(ctx context.Context) {
	if m.rt.ApplicationEnabled == nil {
		return
	}
	rows, err := m.rt.DB.QueryContext(ctx, "SELECT DISTINCT tenant_id,project_id,installation_id FROM eid_notifications WHERE state='queued' AND kind LIKE 'webhook_%'")
	if err != nil {
		return
	}
	var scopes []appkit.Scope
	for rows.Next() {
		s := appkit.Scope{ActorID: "system", ApplicationID: "eid"}
		if rows.Scan(&s.TenantID, &s.ProjectID, &s.InstallationID) == nil {
			scopes = append(scopes, s)
		}
	}
	rows.Close()
	for _, s := range scopes {
		if enabled, err := m.rt.ApplicationEnabled(ctx, s.TenantID, s.ProjectID, "eid"); err == nil && enabled {
			m.notifyWebhooks(s)
		}
	}
}

// Close stops the worker. Rows it has not claimed stay queued for the next
// start. A delivery in flight gets the close grace to finish; after that it
// is cancelled, and its result is still recorded within the 5 s finish budget.
func (m *Module) Close() error {
	o := &m.outbox
	if o.stop == nil {
		return nil
	}
	o.stop()
	grace := time.NewTimer(o.grace)
	defer grace.Stop()
	select {
	case <-o.done:
	case <-grace.C:
		o.abort()
		<-o.done
	}
	return nil
}
