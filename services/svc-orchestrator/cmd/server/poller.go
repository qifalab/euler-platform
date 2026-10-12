package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// The provisioning poller (O4, 03§5.3 "callback and poll are double
// insurance"): every interval it scans the CREATING resources, asks the
// driver how each is doing and settles it exactly as a fulfil replay would
// (settleCreating). Without it a resource whose backend became Ready after
// the fulfil call answered 202 stayed CREATING — and its order FULFILLING —
// until somebody happened to replay the fulfil.
//
// A resource CREATING for longer than the timeout is compensated (reclaimed,
// CREATE_FAILED, FAILED → refund) unless the driver reports it Ready right
// now: a late delivery is still a delivery.
//
// Every replica runs the poller. That is safe because settling is guarded
// (see settle.go): one edge lands per resource, and the compensation claim
// keeps a replica that sees Ready from delivering a resource another replica
// is reclaiming.

const (
	pollIntervalEnv        = "EULER_PROVISION_POLL_INTERVAL"
	creatingTimeoutEnv     = "EULER_PROVISION_CREATING_TIMEOUT"
	defaultPollInterval    = 15 * time.Second
	defaultCreatingTimeout = 30 * time.Minute
	// pollPageSize bounds one ledger read of the scan; a tick pages through
	// all CREATING resources.
	pollPageSize = 100
)

// durationEnv reads a Go duration ("15s", "30m") from env, def when unset.
// Zero, negative or malformed values are a configuration error.
func durationEnv(name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive duration such as 15s or 30m", name, raw)
	}
	return d, nil
}

type provisionPoller struct {
	store    *lifecycleStore
	interval time.Duration
	timeout  time.Duration
	pageSize int
}

func newProvisionPoller(s *lifecycleStore, interval, timeout time.Duration) *provisionPoller {
	return &provisionPoller{store: s, interval: interval, timeout: timeout, pageSize: pollPageSize}
}

// run ticks until ctx is cancelled (shutdown); a tick in progress stops at
// the next resource boundary.
func (p *provisionPoller) run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := p.tick(ctx); n > 0 {
				slog.Info("provisioning poller settled resources", "count", n)
			}
		}
	}
}

// tick scans every CREATING resource once and returns how many it settled.
func (p *provisionPoller) tick(ctx context.Context) int {
	settled, after := 0, ""
	for ctx.Err() == nil {
		page, err := p.store.ledger.ListByState(resource.StateCreating, after, p.pageSize)
		if err != nil {
			slog.Error("provisioning poller scan failed", "err", err)
			return settled
		}
		for _, inst := range page {
			if ctx.Err() != nil {
				return settled
			}
			if p.store.pollOne(ctx, inst, p.timeout) {
				settled++
			}
		}
		if len(page) < p.pageSize {
			break
		}
		after = page[len(page)-1].ResourceID
	}
	return settled
}

// pollOne settles one CREATING resource if it can; true when it did.
func (s *lifecycleStore) pollOne(ctx context.Context, listed *resource.Instance, timeout time.Duration) bool {
	acct, resID := listed.AccountID, listed.ResourceID
	// Fresh reads, row first then attributes (the claim protocol's order):
	// the page may be stale by now.
	inst, err := s.ledger.Get(acct, resID)
	if err != nil || inst == nil || inst.State != resource.StateCreating {
		return false
	}
	attrs, err := s.ledger.Attrs(acct, resID)
	if err != nil {
		slog.Error("provisioning poller: attribute read failed", "resourceId", resID, "err", err)
		return false
	}

	var obs provisionObservation
	force := ""
	if attrs[attrCompensating] == "" {
		age := s.clock().Sub(inst.CreatedAt)
		timedOut := age > timeout
		st, qerr := s.driver.Query(resID)
		if errors.Is(qerr, provision.ErrNotFound) && !timedOut {
			// The backend has no record of it — the dispatch was lost, or an
			// in-memory backend restarted. Re-dispatch, as a fulfil replay
			// would: Apply is idempotent per resource.
			spec := provisionSpec(inst, attrs)
			st, qerr = s.driver.Apply(spec)
			s.recordProvision(resID, acct, spec, st, qerr)
			obs = provisionObservation{observed: true, status: st, err: qerr}
		} else if qerr == nil {
			obs = provisionObservation{observed: true, status: st}
		}
		switch {
		case timedOut && !(obs.observed && obs.err == nil && obs.status.Ready()):
			force = fmt.Sprintf("provisioning timed out after %s", age.Truncate(time.Second))
		case !obs.observed:
			// A transient query failure is not a provisioning failure; the
			// next tick asks again and the timeout backstops it.
			slog.Warn("provisioning poller: driver query failed", "resourceId", resID, "err", qerr)
			return false
		}
	}

	var term prepaidTerm
	if inst.ChargeType == resource.ChargePrepaid && inst.ExpiredAt.IsZero() && force == "" &&
		attrs[attrCompensating] == "" && obs.err == nil && obs.status.Ready() {
		t, ok := termFromAttrs(attrs)
		if !ok {
			// Created before the term was recorded on the resource: ask
			// svc-order, which owns it. Settling without it would sell the
			// term for free (no expiry), so wait for svc-order instead.
			po, oerr := s.orderSrc.Order(ctx, acct, inst.OrderID)
			if oerr != nil {
				slog.Warn("provisioning poller: order lookup for the prepaid term failed", "resourceId", resID,
					"orderId", inst.OrderID, "err", oerr)
				return false
			}
			t = prepaidTerm{Duration: po.Duration, Unit: po.DurationUnit}
		}
		term = t
	}

	res := s.settleCreating(ctx, inst, attrs, obs, force, term)
	switch res.kind {
	case settleDelivered:
		if _, err := s.settleMirror(inst.OrderID, order.StateCompleted); err != nil {
			slog.Error("order mirror transition to COMPLETED failed", "orderId", inst.OrderID, "err", err)
		}
		s.reportFulfilment(ctx, acct, inst.OrderID, res.inst)
		slog.Info("provisioning poller delivered resource", "resourceId", resID, "orderId", inst.OrderID)
		return true
	case settleCompensated:
		if _, err := s.settleMirror(inst.OrderID, order.StateRefunding); err != nil {
			slog.Error("order mirror transition to REFUNDING failed", "orderId", inst.OrderID, "err", err)
		}
		s.reportFulfilment(ctx, acct, inst.OrderID, res.inst)
		slog.Error("provisioning poller compensated resource", "resourceId", resID, "orderId", inst.OrderID, "reason", res.err)
		return true
	case settleReclaimPending, settleRunningFailed:
		slog.Error("provisioning poller could not settle resource; retrying next tick", "resourceId", resID, "err", res.err)
	}
	return false
}
