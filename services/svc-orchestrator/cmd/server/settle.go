package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// Settling a CREATING resource — the one implementation behind the fulfil
// call, its replays and the provisioning poller (O4), so a resource settles
// the same way whoever gets to it first:
//
//	driver Ready       → RUNNING (prepaid expiry stamped), quota committed,
//	                     COMPLETED reported to svc-order
//	driver Failed /    → compensation: claim it (MarkCompensation), reclaim
//	dispatch error /     via driver.Delete, CREATE_FAILED, quota released,
//	CREATING timeout     FAILED reported (svc-order refunds)
//	still booting      → nothing; a replay, the poller or the timeout settles
//
// # Several replicas
//
// Every write is guarded by (status, version), so of two replicas settling one
// resource exactly one edge lands; the loser re-reads and serves the winner's
// outcome. The edges alone are not enough for the compensation, though: its
// reclaim (driver.Delete) happens BEFORE its CREATE_FAILED edge, and a replica
// that saw Ready in between would deliver a resource that is being deleted. So
// the compensation is claimed first — a version bump plus attrCompensating, in
// one write. A settle that read the row before the claim fails its RUNNING
// edge on the version; one that read it after sees the marker (the marker is
// always read after the row) and resumes the compensation instead of
// delivering. A claimer whose reclaim fails revokes its claim — nothing was
// reclaimed, so a later attempt may still deliver — while a claim whose
// reclaim succeeded (or whose claimer died) is resumed by every later attempt
// until CREATE_FAILED lands: the durable successor of the old in-process
// "reclaimed" set, which a restart forgot.

// prepaidTerm is the purchased term a prepaid resource runs for, counted from
// its first RUNNING edge.
type prepaidTerm struct {
	Duration int64
	Unit     pricing.DurationUnit
}

// termAttrs records the term on the resource at create time.
func (t prepaidTerm) attrs() map[string]string {
	if t.Duration <= 0 || t.Unit == "" {
		return nil
	}
	return map[string]string{attrTermDuration: strconv.FormatInt(t.Duration, 10), attrTermUnit: string(t.Unit)}
}

// termFromAttrs reads the recorded term; ok is false when none was recorded.
func termFromAttrs(attrs map[string]string) (prepaidTerm, bool) {
	d, err := strconv.ParseInt(attrs[attrTermDuration], 10, 64)
	if err != nil || d <= 0 || attrs[attrTermUnit] == "" {
		return prepaidTerm{}, false
	}
	return prepaidTerm{Duration: d, Unit: pricing.DurationUnit(attrs[attrTermUnit])}, true
}

// provisionSpec is the declarative spec for a resource: its row plus its
// customer attributes as Params. Built the same way by the fulfil call, its
// replays and the poller's re-dispatch, so Apply sees one spec per resource.
func provisionSpec(inst *resource.Instance, attrs map[string]string) provision.Spec {
	var params map[string]string
	if pub := publicAttrs(attrs); len(pub) > 0 {
		params = pub
	}
	resType := inst.ResourceType
	if resType == "" {
		resType = "instance"
	}
	return provision.Spec{
		ResourceID: inst.ResourceID, AccountID: inst.AccountID, ProjectID: inst.ProjectID,
		ProductCode: inst.ProductCode, ResourceType: resType, Region: inst.Region, Zone: inst.Zone,
		Params: params, IdempotencyKey: strconv.FormatInt(inst.OrderID, 10),
	}
}

// provisionObservation is what the driver said about a CREATING resource.
type provisionObservation struct {
	// observed is false when the driver was not asked (the attempt found a
	// compensation claim and resumes it instead of dispatching).
	observed bool
	status   provision.Status
	err      error
}

type settleKind int

const (
	settleInProgress     settleKind = iota // still provisioning (or not observed): CREATING
	settleDelivered                        // CREATING → RUNNING
	settleCompensated                      // reclaimed, CREATING → CREATE_FAILED
	settleReclaimPending                   // compensation could not finish: still CREATING
	settleRunningFailed                    // the RUNNING edge did not land: still CREATING
	settledElsewhere                       // another attempt settled it first
)

// settleResult is the outcome; inst is the row as it now stands (for
// settledElsewhere, the winner's row).
type settleResult struct {
	kind settleKind
	inst *resource.Instance
	err  error // the failure behind a compensation or a failed edge, for logs
}

// settleCreating settles inst, a CREATING row read just now, whose attributes
// attrs were read AFTER it (the claim protocol above depends on that order).
// force, when set, is a compensation reason that overrides the observation
// (the CREATING timeout).
func (s *lifecycleStore) settleCreating(ctx context.Context, inst *resource.Instance, attrs map[string]string,
	obs provisionObservation, force string, term prepaidTerm) settleResult {
	reason := attrs[attrCompensating]
	resuming := reason != ""
	if !resuming {
		switch {
		case force != "":
			reason = force
		case !obs.observed:
			// Skipped the dispatch for a claim that has since been revoked:
			// nothing new is known. The next attempt dispatches.
			return settleResult{kind: settleInProgress, inst: inst}
		case obs.err != nil:
			reason = "provision failed: " + obs.err.Error()
		case obs.status.Ready():
			return s.deliver(ctx, inst, attrs, term)
		case provisionInProgress(obs.status):
			return settleResult{kind: settleInProgress, inst: inst}
		default:
			reason = fmt.Sprintf("driver reported phase %q", obs.status.Phase)
		}
		if err := s.ledger.MarkCompensation(inst, reason, true); err != nil {
			if cur := s.settledRow(inst); cur != nil {
				return settleResult{kind: settledElsewhere, inst: cur}
			}
			// Still CREATING but moved under us: another attempt holds the
			// claim (or the ledger failed). Either way nothing was reclaimed
			// here; a later attempt resumes.
			return settleResult{kind: settleReclaimPending, inst: inst, err: fmt.Errorf("claim compensation: %w", err)}
		}
	}

	// Compensation (reverse order): reclaim whatever the driver may have
	// created — Delete is idempotent, so a resource that never came up is safe
	// to reclaim — then record the rollback.
	if err := s.driver.Delete(inst.ResourceID); err != nil {
		if !resuming {
			// Not reclaimed: CREATE_FAILED means "rolled back", and FAILED
			// would refund a machine that may still be running. Revoke our
			// own claim so a retry may re-drive the provisioning; the
			// timeout claims it again if the backend stays broken.
			if rerr := s.ledger.MarkCompensation(inst, "reclaim failed: "+err.Error(), false); rerr != nil {
				slog.Error("revoking the compensation claim failed; later attempts resume the compensation",
					"resourceId", inst.ResourceID, "err", rerr)
			}
		}
		return settleResult{kind: settleReclaimPending, inst: inst, err: fmt.Errorf("%s; reclaim failed: %w", reason, err)}
	}
	if err := s.ledger.Transition(inst, resource.StateCreateFailed, truncateRunes(reason, stateLogReasonMax)); err != nil {
		if cur := s.settledRow(inst); cur != nil {
			return settleResult{kind: settledElsewhere, inst: cur}
		}
		// Reclaimed, but the ledger still says CREATING: the claim stays, so
		// every later attempt resumes this compensation — retries the edge
		// and then reports FAILED — instead of provisioning again.
		return settleResult{kind: settleReclaimPending, inst: inst, err: fmt.Errorf("%s; CREATE_FAILED edge: %w", reason, err)}
	}
	s.releaseQuota(ctx, inst, attrs)
	return settleResult{kind: settleCompensated, inst: inst, err: errors.New(reason)}
}

// deliver takes the RUNNING edge. BillingStart is stamped by the ledger on
// it (计费起点=首次 RUNNING 时刻); a prepaid term runs from the same moment
// for the duration the order paid for.
func (s *lifecycleStore) deliver(ctx context.Context, inst *resource.Instance, attrs map[string]string, term prepaidTerm) settleResult {
	if inst.ChargeType == resource.ChargePrepaid && inst.ExpiredAt.IsZero() {
		inst.ExpiredAt = prepaidExpiry(s.clock(), term.Duration, term.Unit)
	}
	if err := s.ledger.Transition(inst, resource.StateRunning, "provisioned"); err != nil {
		if cur := s.settledRow(inst); cur != nil {
			return settleResult{kind: settledElsewhere, inst: cur}
		}
		return settleResult{kind: settleRunningFailed, inst: inst, err: err}
	}
	s.commitQuota(ctx, inst, attrs)
	return settleResult{kind: settleDelivered, inst: inst}
}

// settledRow re-reads inst after a guarded write lost: the row when another
// attempt has settled it (it is no longer CREATING), nil otherwise.
func (s *lifecycleStore) settledRow(inst *resource.Instance) *resource.Instance {
	cur, err := s.ledger.Get(inst.AccountID, inst.ResourceID)
	if err != nil || cur == nil || cur.State == resource.StateCreating {
		return nil
	}
	return cur
}

// --- the order mirror -----------------------------------------------------------

// settleMirror moves this process's order mirror (if it has one, FULFILLING)
// to `to` and returns the mirror's state afterwards ("" without a mirror).
func (s *lifecycleStore) settleMirror(orderID int64, to order.State) (order.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[orderID]
	if !ok {
		return "", nil
	}
	if o.State == order.StateFulfilling {
		if _, err := order.NewMachine(time.Now).Transition(o, to, o.Version); err != nil {
			return o.State, err
		}
	}
	return o.State, nil
}

// mirrorState reads the mirror's state under the lock.
func (s *lifecycleStore) mirrorState(orderID int64) order.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.orders[orderID]; ok {
		return o.State
	}
	return ""
}

// dropMirror forgets the mirror, so the next attempt re-mirrors the order
// from svc-order instead of 409-ing on a stale FULFILLING copy.
func (s *lifecycleStore) dropMirror(orderID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.orders, orderID)
}

// --- quota (O6) -------------------------------------------------------------------

// commitQuota turns the resource's reservation into used capacity once it is
// RUNNING. Best effort: a failure is logged (the resource is delivered either
// way) and retried by the next fulfil replay (healQuota). A reservation that
// expired while the resource booted (svc-quota's TTL is shorter than the
// CREATING timeout) is replaced by a fresh one committed at once, so the
// running resource still counts.
func (s *lifecycleStore) commitQuota(ctx context.Context, inst *resource.Instance, attrs map[string]string) {
	resv := attrs[attrQuotaReservation]
	if s.quota == nil || resv == "" || attrs[attrQuotaState] == quotaCommitted {
		return
	}
	ctx = context.WithoutCancel(ctx)
	err := s.quota.Commit(ctx, inst.AccountID, resv)
	if errors.Is(err, errReservationGone) {
		late, oerr := s.occupyActive(ctx, inst.AccountID, quotaOccupyFor(inst, inst.OrderID, "-running"))
		if oerr != nil {
			slog.Error("resource is RUNNING but its quota reservation lapsed and could not be replaced; usage is under-counted",
				"resourceId", inst.ResourceID, "reservationId", resv, "err", oerr)
			return
		}
		resv, err = late, s.quota.Commit(ctx, inst.AccountID, late)
	}
	if err != nil {
		slog.Error("quota commit failed; a fulfil replay retries it", "resourceId", inst.ResourceID, "reservationId", resv, "err", err)
		return
	}
	s.setQuotaState(inst, resv, quotaCommitted)
}

// releaseQuota returns the reservation of a resource that was never
// delivered. A committed reservation is used capacity, which freeQuota gives
// back when the resource is released.
func (s *lifecycleStore) releaseQuota(ctx context.Context, inst *resource.Instance, attrs map[string]string) {
	resv := attrs[attrQuotaReservation]
	if s.quota == nil || resv == "" || attrs[attrQuotaState] == quotaReleased || attrs[attrQuotaState] == quotaCommitted {
		return
	}
	if err := s.quota.Release(context.WithoutCancel(ctx), inst.AccountID, resv); err != nil {
		// svc-quota sweeps an uncommitted reservation at its TTL anyway.
		slog.Error("quota release failed; svc-quota's TTL sweep reclaims it", "resourceId", inst.ResourceID,
			"reservationId", resv, "err", err)
		return
	}
	s.setQuotaState(inst, resv, quotaReleased)
}

func (s *lifecycleStore) setQuotaState(inst *resource.Instance, resv, state string) {
	if err := s.ledger.UpdateAttrs(inst.AccountID, inst.ResourceID, attrUpdate{
		Set: map[string]string{attrQuotaReservation: resv, attrQuotaState: state},
	}); err != nil {
		slog.Error("recording the quota reservation state failed", "resourceId", inst.ResourceID,
			"reservationId", resv, "state", state, "err", err)
	}
}

// freeQuota gives a released resource's used capacity back to the account
// (svc-quota POST /internal/quota/free) — the release saga's quota step, run
// between the reclaim and the RELEASED edge. The free is keyed by the
// resource id, so a release retried after a failed free applies it once. A
// failure is returned: the release stays RELEASING and its retry frees again,
// rather than leaving the account charged a slot for a resource it no longer
// has. A reservation that was never committed is just released.
func (s *lifecycleStore) freeQuota(ctx context.Context, inst *resource.Instance) error {
	if s.quota == nil {
		return nil
	}
	attrs, err := s.ledger.Attrs(inst.AccountID, inst.ResourceID)
	if err != nil {
		return err
	}
	switch attrs[attrQuotaState] {
	case quotaCommitted:
	case quotaOccupied:
		s.releaseQuota(ctx, inst, attrs)
		return nil
	default:
		return nil // nothing held (released, freed, or never reserved)
	}
	resType := inst.ResourceType
	if resType == "" {
		resType = "instance"
	}
	err = s.quota.Free(context.WithoutCancel(ctx), inst.AccountID, quotaFreeRequest{
		ProductCode: inst.ProductCode, ResourceType: resType, Region: inst.Region, Amount: 1,
		BizKey: inst.ResourceID, IdempotencyKey: inst.ResourceID,
	})
	if err != nil && !errors.Is(err, errQuotaUnmodelled) {
		return fmt.Errorf("quota free for %s: %w", inst.ResourceID, err)
	}
	s.setQuotaState(inst, attrs[attrQuotaReservation], quotaFreed)
	return nil
}

// maxQuotaKeyChain bounds occupyActive's walk: each link is a reservation of
// this order that ended without delivering a resource (a refused create the
// caller retried after svc-quota's TTL swept it). Past it the order needs an
// operator, not another reservation.
const maxQuotaKeyChain = 8

// occupyActive occupies under base.IdempotencyKey and, while svc-quota
// replays a reservation of that key which has already ended, under the next
// key of a deterministic chain (key, key#2, key#3, ...): every attempt at one
// order — retries, replicas, restarts — walks the same chain and lands on the
// same live reservation.
func (s *lifecycleStore) occupyActive(ctx context.Context, acct int64, base quotaOccupyRequest) (string, error) {
	req := base
	for i := 1; i <= maxQuotaKeyChain; i++ {
		if i > 1 {
			req.IdempotencyKey = base.IdempotencyKey + "#" + strconv.Itoa(i)
			req.BizKey = base.BizKey + "#" + strconv.Itoa(i)
		}
		r, err := s.quota.Occupy(ctx, acct, req)
		if err != nil {
			return "", err
		}
		if r.Active {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("svc-quota: %d reservations of key %q have ended", maxQuotaKeyChain, base.IdempotencyKey)
}

// healQuota finishes a quota step a settled resource still owes (a commit or
// release whose call failed): run on every fulfil replay of a settled order.
func (s *lifecycleStore) healQuota(ctx context.Context, inst *resource.Instance) {
	if s.quota == nil || inst.State == resource.StateCreating {
		return
	}
	attrs, err := s.ledger.Attrs(inst.AccountID, inst.ResourceID)
	if err != nil || attrs[attrQuotaReservation] == "" {
		return
	}
	switch inst.State {
	case resource.StateCreateFailed, resource.StateReleasing, resource.StateReleased:
		s.releaseQuota(ctx, inst, attrs)
	default:
		s.commitQuota(ctx, inst, attrs)
	}
}

// quotaOccupyFor is the occupy request for one resource of an order. The
// idempotency key is the order id (plus suffix for the late replacement), so
// every attempt at one order holds one reservation.
func quotaOccupyFor(inst *resource.Instance, orderID int64, suffix string) quotaOccupyRequest {
	key := strconv.FormatInt(orderID, 10) + suffix
	resType := inst.ResourceType
	if resType == "" {
		resType = "instance"
	}
	return quotaOccupyRequest{
		ProductCode: inst.ProductCode, ResourceType: resType, Region: inst.Region, Count: 1,
		BizKey: "order-" + key, IdempotencyKey: key,
	}
}
