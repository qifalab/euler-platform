package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// fakeQuota is svc-quota: reservations by id, with injectable failures.
type fakeQuota struct {
	mu        sync.Mutex
	seq       int
	byKey     map[string]string // idempotency key → reservation id
	occupied  map[string]bool   // live reservations
	committed []string
	released  []string
	freed     map[string]int // idempotency key → applied frees (svc-quota applies a key once)
	frees     int            // free calls received
	occupies  []quotaOccupyRequest
	occupyErr error
	commitErr error // returned once, then cleared
	freeErr   error
}

func newFakeQuota() *fakeQuota {
	return &fakeQuota{byKey: map[string]string{}, occupied: map[string]bool{}, freed: map[string]int{}}
}

// Occupy mirrors svc-quota's durable keys: a known key replays its
// reservation, active only while it is still pending.
func (q *fakeQuota) Occupy(_ context.Context, _ int64, r quotaOccupyRequest) (quotaReservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.occupies = append(q.occupies, r)
	if q.occupyErr != nil {
		return quotaReservation{}, q.occupyErr
	}
	if id, ok := q.byKey[r.IdempotencyKey]; ok {
		return quotaReservation{ID: id, Active: q.occupied[id]}, nil
	}
	q.seq++
	id := fmt.Sprintf("qt-%d", q.seq)
	q.byKey[r.IdempotencyKey], q.occupied[id] = id, true
	return quotaReservation{ID: id, Active: true}, nil
}

func (q *fakeQuota) Free(_ context.Context, _ int64, r quotaFreeRequest) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.frees++
	if q.freeErr != nil {
		return q.freeErr
	}
	if r.Amount != 1 || r.BizKey == "" || r.IdempotencyKey == "" {
		return fmt.Errorf("bad free request %+v", r)
	}
	if q.freed[r.IdempotencyKey] == 0 {
		q.freed[r.IdempotencyKey] = 1
	}
	return nil
}

func (q *fakeQuota) Commit(_ context.Context, _ int64, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.commitErr; err != nil {
		q.commitErr = nil
		return err
	}
	if !q.occupied[id] {
		return errReservationGone
	}
	delete(q.occupied, id)
	q.committed = append(q.committed, id)
	return nil
}

func (q *fakeQuota) Release(_ context.Context, _ int64, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.occupied, id)
	q.released = append(q.released, id)
	return nil
}

func (q *fakeQuota) snapshot() (occupies []quotaOccupyRequest, committed, released []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]quotaOccupyRequest(nil), q.occupies...), append([]string(nil), q.committed...), append([]string(nil), q.released...)
}

// O6: occupy (idempotency key = order id) before provisioning, commit once
// RUNNING, recorded on the resource; a replay does not occupy again.
func TestFulfilOccupiesAndCommitsQuota(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		q := newFakeQuota()
		s.quota = q
		o := paidIn(601, "euecs", "cn-east-1")
		orders.add(100123, o)
		rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, nil))
		if rec.Code != 200 {
			t.Fatalf("fulfil: %d %s", rec.Code, rec.Body.String())
		}
		resID := decodeFulfil(t, rec).Data.ResourceID
		occ, committed, _ := q.snapshot()
		if len(occ) != 1 || occ[0] != (quotaOccupyRequest{ProductCode: "euecs", ResourceType: "instance", Region: "cn-east-1",
			Count: 1, BizKey: "order-601", IdempotencyKey: "601"}) {
			t.Fatalf("occupy = %+v", occ)
		}
		if len(committed) != 1 || committed[0] != "qt-1" {
			t.Fatalf("committed = %v", committed)
		}
		attrs, _ := s.ledger.Attrs(100123, resID)
		if attrs[attrQuotaReservation] != "qt-1" || attrs[attrQuotaState] != quotaCommitted {
			t.Fatalf("quota bookkeeping = %v", attrs)
		}
		if v := decodeView(t, detailAs(t, s, 100123, resID)); len(v.Attributes) != 0 {
			t.Fatalf("quota bookkeeping leaked into the API: %v", v.Attributes)
		}
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, nil)); rec.Code != 200 {
			t.Fatalf("replay: %d", rec.Code)
		}
		if occ, committed, _ := q.snapshot(); len(occ) != 1 || len(committed) != 1 {
			t.Fatalf("a replay occupied/committed again: %+v %v", occ, committed)
		}
	})
}

// O6: an exceeded quota fails the order before anything is provisioned: 409
// Quota.Exceeded, FAILED reported (svc-order refunds), no resource, no
// dispatch. svc-quota being down fails closed with 503 and leaves the order
// PAID; a product svc-quota models no quota for proceeds.
func TestFulfilQuotaRefusals(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		q := newFakeQuota()
		s.quota = q
		d := newScriptedDriver()
		s.driver = d
		exceeded, down, unmodelled := paidIn(611, "euecs", "cn-north-1"), paidIn(612, "euecs", "cn-north-1"), paidIn(613, "eueip", "cn-north-1")
		for _, o := range []paidOrder{exceeded, down, unmodelled} {
			orders.add(100123, o)
		}
		before := len(mustList(t, s, 100123))

		q.occupyErr = fmt.Errorf("%w: quota_euecs_instance available 0, requested 1", errQuotaExceeded)
		wantCode(t, "quota exceeded", fulfilAs(t, s, 100123, fulfilBodyFor(exceeded, nil)), 409, "Quota.Exceeded")
		reps := reportsOf(s).calls()
		if len(reps) != 1 || reps[0].orderID != 611 || reps[0].rep.Result != fulfilmentFailed || !strings.Contains(reps[0].rep.Reason, "quota") {
			t.Fatalf("reports = %+v, want one FAILED (refund) for 611", reps)
		}

		q.occupyErr = errors.New("dial tcp: connection refused")
		wantCode(t, "quota service down", fulfilAs(t, s, 100123, fulfilBodyFor(down, nil)), 503, "Quota.Unavailable")
		if len(reportsOf(s).calls()) != 1 {
			t.Fatal("an unreachable svc-quota must not fail the order")
		}
		if n := len(mustList(t, s, 100123)); n != before || len(d.applied) != 0 {
			t.Fatalf("refused fulfils provisioned: %d resources (was %d), %d applies", n, before, len(d.applied))
		}

		q.occupyErr = errQuotaUnmodelled
		d.next = provision.PhaseReady
		rec := fulfilAs(t, s, 100123, fulfilBodyFor(unmodelled, nil))
		if rec.Code != 200 {
			t.Fatalf("product without a quota: %d %s", rec.Code, rec.Body.String())
		}
		attrs, _ := s.ledger.Attrs(100123, decodeFulfil(t, rec).Data.ResourceID)
		if attrs[attrQuotaReservation] != "" {
			t.Fatalf("a reservation was recorded for an unmodelled quota: %v", attrs)
		}

		// The order svc-quota was down for is still PAID: the retry goes through.
		q.occupyErr = nil
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(down, nil)); rec.Code != 200 {
			t.Fatalf("retry once svc-quota is back: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// O6: a compensated provisioning releases its reservation; a reservation that
// lapsed while the resource booted is replaced and committed; a commit that
// failed is finished by the next replay.
func TestQuotaReleaseAndHealing(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		q := newFakeQuota()
		s.quota = q
		d := newScriptedDriver()
		s.driver = d
		failed, lapsed, flaky := paidIn(621, "euecs", "cn-north-1"), paidIn(622, "euecs", "cn-north-1"), paidIn(623, "euecs", "cn-north-1")
		for _, o := range []paidOrder{failed, lapsed, flaky} {
			orders.add(100123, o)
		}

		d.applyErr = errors.New("rc-compute: no capacity")
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(failed, nil)); rec.Code != 500 {
			t.Fatalf("failed provisioning: %d %s", rec.Code, rec.Body.String())
		}
		resID := fulfilResourceID("euecs", "cn-north-1", 100123, 621)
		if _, _, released := q.snapshot(); len(released) != 1 || released[0] != "qt-1" {
			t.Fatalf("released = %v, want the compensation to release qt-1", released)
		}
		if attrs, _ := s.ledger.Attrs(100123, resID); attrs[attrQuotaState] != quotaReleased {
			t.Fatalf("quota state = %q", attrs[attrQuotaState])
		}

		// Lapsed reservation: svc-quota swept it while the resource booted.
		d.applyErr, d.next = nil, provision.PhaseProvisioning
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(lapsed, nil)); rec.Code != 202 {
			t.Fatalf("lapsed: %d %s", rec.Code, rec.Body.String())
		}
		lapsedID := fulfilResourceID("euecs", "cn-north-1", 100123, 622)
		q.mu.Lock()
		delete(q.occupied, "qt-2")
		q.mu.Unlock()
		d.set(lapsedID, provision.PhaseReady)
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(lapsed, nil)); rec.Code != 200 {
			t.Fatalf("lapsed retry: %d %s", rec.Code, rec.Body.String())
		}
		occ, committed, _ := q.snapshot()
		if last := occ[len(occ)-1]; last.IdempotencyKey != "622-running" || len(committed) != 1 || committed[0] != "qt-3" {
			t.Fatalf("occupies %+v, committed %v: want a replacement reservation committed", occ, committed)
		}
		if attrs, _ := s.ledger.Attrs(100123, lapsedID); attrs[attrQuotaReservation] != "qt-3" || attrs[attrQuotaState] != quotaCommitted {
			t.Fatalf("bookkeeping = %v", attrs)
		}

		// A commit that failed is retried by the next replay.
		d.next = provision.PhaseReady
		q.commitErr = errors.New("svc-quota: 503")
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(flaky, nil)); rec.Code != 200 {
			t.Fatalf("flaky commit: %d %s", rec.Code, rec.Body.String())
		}
		flakyID := fulfilResourceID("euecs", "cn-north-1", 100123, 623)
		if attrs, _ := s.ledger.Attrs(100123, flakyID); attrs[attrQuotaState] != quotaOccupied {
			t.Fatalf("after a failed commit: %v", attrs)
		}
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(flaky, nil)); rec.Code != 200 {
			t.Fatalf("replay: %d", rec.Code)
		}
		if attrs, _ := s.ledger.Attrs(100123, flakyID); attrs[attrQuotaState] != quotaCommitted {
			t.Fatalf("the replay did not finish the commit: %v", attrs)
		}
	})
}

// O6: releasing a delivered resource gives its used capacity back, keyed by
// the resource; a failed free keeps the release RELEASING and its retry
// completes it, applying the free once.
func TestReleaseFreesCommittedQuota(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		q := newFakeQuota()
		s.quota = q
		o := paidIn(631, "euecs", "cn-north-1")
		orders.add(100123, o)
		resID := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(o, nil))).Data.ResourceID

		q.freeErr = errors.New("svc-quota: 503")
		wantCode(t, "release while svc-quota is down", releaseAs(t, s, 100123, resID), 500, "Resource.ReleaseFailed")
		if got := mustGet(t, s.ledger, resID); got.State != resource.StateReleasing {
			t.Fatalf("state = %s, want RELEASING until the free lands", got.State)
		}
		q.freeErr = nil
		if rec := releaseAs(t, s, 100123, resID); rec.Code != 200 {
			t.Fatalf("release retry: %d %s", rec.Code, rec.Body.String())
		}
		if got := mustGet(t, s.ledger, resID); got.State != resource.StateReleased {
			t.Fatalf("state = %s", got.State)
		}
		if q.frees != 2 || q.freed[resID] != 1 {
			t.Fatalf("frees: %d calls, applied %v; want the retry keyed by %s", q.frees, q.freed, resID)
		}
		if attrs, _ := s.ledger.Attrs(100123, resID); attrs[attrQuotaState] != quotaFreed {
			t.Fatalf("quota state = %q", attrs[attrQuotaState])
		}

		// A rolled-back resource never held used capacity: its release frees nothing.
		d := newScriptedDriver()
		d.applyErr = errors.New("no capacity")
		s.driver = d
		f := paidIn(632, "euecs", "cn-north-1")
		orders.add(100123, f)
		_ = fulfilAs(t, s, 100123, fulfilBodyFor(f, nil))
		failedID := fulfilResourceID("euecs", "cn-north-1", 100123, 632)
		if rec := releaseAs(t, s, 100123, failedID); rec.Code != 200 {
			t.Fatalf("release CREATE_FAILED: %d %s", rec.Code, rec.Body.String())
		}
		if q.frees != 2 {
			t.Fatalf("a never-delivered resource was freed (%d calls)", q.frees)
		}
	})
}

// O6: svc-quota keys are durable — a key whose reservation ended replays it
// as inactive — so a retry long after a refused create moves to the next key
// of the order's chain instead of provisioning on a dead reservation.
func TestOccupyMovesPastEndedReservation(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		q := newFakeQuota()
		s.quota = q
		taken, again := paidIn(641, "euoss", "cn-north-1"), paidIn(642, "euoss", "cn-north-1")
		orders.add(100123, taken)
		orders.add(200456, again)
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(taken, map[string]any{"bucket": "chain-test"})); rec.Code != 200 {
			t.Fatalf("first bucket: %d %s", rec.Code, rec.Body.String())
		}
		wantCode(t, "taken bucket", fulfilAs(t, s, 200456, fulfilBodyFor(again, map[string]any{"bucket": "chain-test"})), 409, "Resource.BucketExists")
		// svc-quota's TTL sweep ends the refused attempt's reservation.
		q.mu.Lock()
		dead := q.byKey["642"]
		delete(q.occupied, dead)
		q.mu.Unlock()
		rec := fulfilAs(t, s, 200456, fulfilBodyFor(again, map[string]any{"bucket": "chain-test-2"}))
		if rec.Code != 200 {
			t.Fatalf("retry with another bucket: %d %s", rec.Code, rec.Body.String())
		}
		attrs, _ := s.ledger.Attrs(200456, decodeFulfil(t, rec).Data.ResourceID)
		occ, committed, _ := q.snapshot()
		if last := occ[len(occ)-1]; last.IdempotencyKey != "642#2" || attrs[attrQuotaReservation] == dead ||
			attrs[attrQuotaState] != quotaCommitted || committed[len(committed)-1] != attrs[attrQuotaReservation] {
			t.Fatalf("occupies %+v, bookkeeping %v: want a fresh reservation under 642#2", occ, attrs)
		}
	})
}

func TestHTTPQuotaClient(t *testing.T) {
	type seen struct{ path, acct, token, body string }
	var mu sync.Mutex
	var got []seen
	reply := map[string]struct {
		status int
		body   string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.URL.Path, r.Header.Get(accountIDHeader), r.Header.Get("X-Euler-Internal-Token"), string(raw)})
		rp := reply[r.URL.Path]
		mu.Unlock()
		w.WriteHeader(rp.status)
		_, _ = w.Write([]byte(rp.body))
	}))
	defer srv.Close()
	t.Setenv(quotaURLEnv, "")
	if newQuotaReserverFromEnv() != nil {
		t.Fatal("no URL must mean quota not enforced (nil client)")
	}
	t.Setenv(quotaURLEnv, srv.URL+"/")
	t.Setenv("EULER_INTERNAL_TOKEN", "s3cret")
	c := newQuotaReserverFromEnv()
	ctx := context.Background()

	reply["/api/v1/quota/occupy"] = struct {
		status int
		body   string
	}{200, `{"Code":"OK","Data":{"reservationId":"qt-9","quotaCode":"quota_euecs_instance","replayed":false,"active":true}}`}
	res, err := c.Occupy(ctx, 100123, quotaOccupyRequest{ProductCode: "euecs", ResourceType: "instance", Region: "cn-north-1", Count: 1, BizKey: "order-7", IdempotencyKey: "7"})
	if err != nil || res != (quotaReservation{ID: "qt-9", Active: true}) {
		t.Fatalf("occupy = %+v %v", res, err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(got[0].body), &body)
	if got[0].acct != "100123" || got[0].token != "s3cret" || body["idempotencyKey"] != "7" || body["count"] != float64(1) || body["productCode"] != "euecs" {
		t.Fatalf("occupy request = %+v", got[0])
	}
	reply["/api/v1/quota/occupy"] = struct {
		status int
		body   string
	}{200, `{"Code":"OK","Data":{"reservationId":"qt-9","replayed":true,"active":false}}`}
	if res, err := c.Occupy(ctx, 100123, quotaOccupyRequest{ProductCode: "euecs", Count: 1, IdempotencyKey: "7"}); err != nil || res.Active {
		t.Fatalf("replay of an ended reservation = %+v %v, want inactive", res, err)
	}
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{409, `{"Code":"Quota.Exceeded","Message":"available 0"}`, errQuotaExceeded},
		{404, `{"Code":"Quota.UnknownQuota","Message":"no such quota code"}`, errQuotaUnmodelled},
		{500, `{"Code":"Quota.OccupyFailed"}`, nil},
	} {
		reply["/api/v1/quota/occupy"] = struct {
			status int
			body   string
		}{tc.status, tc.body}
		_, err := c.Occupy(ctx, 100123, quotaOccupyRequest{ProductCode: "euecs", Count: 1})
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) ||
			(tc.want == nil && (errors.Is(err, errQuotaExceeded) || errors.Is(err, errQuotaUnmodelled))) {
			t.Fatalf("occupy %d %s: %v", tc.status, tc.body, err)
		}
	}

	reply["/api/v1/quota/commit"] = struct {
		status int
		body   string
	}{200, `{"Code":"OK","Data":{"committed":true}}`}
	if err := c.Commit(ctx, 100123, "qt-9"); err != nil {
		t.Fatal(err)
	}
	if last := got[len(got)-1]; last.path != "/api/v1/quota/commit" || !strings.Contains(last.body, `"reservationId":"qt-9"`) {
		t.Fatalf("commit request = %+v", last)
	}
	for _, b := range []string{`{"Code":"Quota.NotFound"}`, `{"Code":"Quota.ReservationExpired"}`} {
		reply["/api/v1/quota/commit"] = struct {
			status int
			body   string
		}{409, b}
		if err := c.Commit(ctx, 100123, "qt-9"); !errors.Is(err, errReservationGone) {
			t.Fatalf("commit %s: %v, want errReservationGone", b, err)
		}
	}
	reply["/api/v1/quota/release"] = struct {
		status int
		body   string
	}{200, `{"Code":"OK","Data":{"released":true}}`}
	if err := c.Release(ctx, 100123, "qt-9"); err != nil {
		t.Fatal(err)
	}
	reply["/api/v1/quota/release"] = struct {
		status int
		body   string
	}{500, `{"Code":"Quota.ReleaseFailed"}`}
	if err := c.Release(ctx, 100123, "qt-9"); err == nil {
		t.Fatal("a failed release reported success")
	}

	reply["/internal/quota/free"] = struct {
		status int
		body   string
	}{200, `{"Code":"OK","Data":{"freed":true,"replayed":false}}`}
	free := quotaFreeRequest{ProductCode: "euecs", ResourceType: "instance", Region: "cn-north-1", Amount: 1,
		BizKey: "euecs-cn-north-1-01-00000007", IdempotencyKey: "euecs-cn-north-1-01-00000007"}
	if err := c.Free(ctx, 100123, free); err != nil {
		t.Fatal(err)
	}
	last := got[len(got)-1]
	var fb quotaFreeRequest
	_ = json.Unmarshal([]byte(last.body), &fb)
	if last.path != "/internal/quota/free" || last.acct != "100123" || last.token != "s3cret" || fb != free {
		t.Fatalf("free request = %+v (%+v)", last, fb)
	}
	reply["/internal/quota/free"] = struct {
		status int
		body   string
	}{404, `{"Code":"Quota.UnknownQuota"}`}
	if err := c.Free(ctx, 100123, free); !errors.Is(err, errQuotaUnmodelled) {
		t.Fatalf("free of an unmodelled quota: %v", err)
	}
	reply["/internal/quota/free"] = struct {
		status int
		body   string
	}{500, `{"Code":"Quota.FreeFailed"}`}
	if err := c.Free(ctx, 100123, free); err == nil || errors.Is(err, errQuotaUnmodelled) {
		t.Fatalf("failed free: %v", err)
	}
}

// --- O5: the order's zone -------------------------------------------------------------

func TestFulfilZoneFromOrder(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		zoned, bare, clash, foreign := paidIn(701, "euecs", "cn-north-1"), paidIn(702, "euecs", "cn-north-1"),
			paidIn(703, "euecs", "cn-north-1"), paidIn(704, "euecs", "cn-north-1")
		zoned.ZoneID, clash.ZoneID = "cn-north-1-b", "cn-north-1-a"
		for _, o := range []paidOrder{zoned, bare, clash, foreign} {
			orders.add(100123, o)
		}
		rec := fulfilAs(t, s, 100123, map[string]any{"orderId": 701, "productCode": "euecs"})
		if v := decodeView(t, detailAs(t, s, 100123, decodeFulfil(t, rec).Data.ResourceID)); v.Zone != "cn-north-1-b" {
			t.Fatalf("zone from the order = %q", v.Zone)
		}
		// No zone on the order: the body's zone (or its zoneId alias) fills in.
		rec = fulfilAs(t, s, 100123, map[string]any{"orderId": 702, "productCode": "euecs", "zoneId": "cn-north-1-c"})
		if v := decodeView(t, detailAs(t, s, 100123, decodeFulfil(t, rec).Data.ResourceID)); v.Zone != "cn-north-1-c" {
			t.Fatalf("zone from the body = %q", v.Zone)
		}
		wantCode(t, "body zone contradicts the order", fulfilAs(t, s, 100123,
			map[string]any{"orderId": 703, "productCode": "euecs", "zone": "cn-north-1-b"}), 409, "Order.ZoneMismatch")
		wantCode(t, "zone of another region", fulfilAs(t, s, 100123,
			map[string]any{"orderId": 704, "productCode": "euecs", "zone": "cn-east-1-a"}), 400, "Common.InvalidParameter")
		wantCode(t, "zone and zoneId disagree", fulfilAs(t, s, 100123,
			map[string]any{"orderId": 704, "productCode": "euecs", "zone": "cn-north-1-a", "zoneId": "cn-north-1-b"}), 400, "Common.InvalidParameter")
		// Agreeing with the order is fine.
		if rec := fulfilAs(t, s, 100123, map[string]any{"orderId": 703, "productCode": "euecs", "zone": "cn-north-1-a"}); rec.Code != 200 {
			t.Fatalf("matching zone: %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHTTPOrderSourceReadsZone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": map[string]any{
			"orderId": 77, "orderNo": "SO77", "state": "PAID", "productCode": "euecs", "skuCode": "euecs.c1",
			"regionId": "cn-north-1", "zoneId": "cn-north-1-b", "chargeType": "PREPAID", "payableAmount": "1",
		}})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_ORDER_URL", srv.URL)
	o, err := newHTTPOrderSource().Order(context.Background(), 100123, 77)
	if err != nil || o.ZoneID != "cn-north-1-b" {
		t.Fatalf("order = %+v, %v", o, err)
	}
}

func TestZoneInRegion(t *testing.T) {
	for zone, ok := range map[string]bool{
		"cn-north-1-a": true, "cn-north-1-az1": true, "cn-north-1-": false, "cn-north-1": false,
		"cn-east-1-a": false, "cn-north-1-A": false, "cn-north-1-a b": false,
	} {
		if zoneInRegion(zone, "cn-north-1") != ok {
			t.Fatalf("zoneInRegion(%q) = %v", zone, !ok)
		}
	}
}

// --- O7: a restarted VM driver still reclaims what it applied --------------------------

func TestVMDriverReleaseAfterRestart(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		cluster := provision.NewMemoryVMClient(0)
		s.driver = provision.NewVMDriverWithSpecs(cluster, nil, s.ledger)
		o := paidIn(801, "euecs", "cn-north-1")
		orders.add(100123, o)
		rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, map[string]any{"name": "vm-1"}))
		if rec.Code != 200 {
			t.Fatalf("fulfil on the VM driver: %d %s", rec.Code, rec.Body.String())
		}
		resID := decodeFulfil(t, rec).Data.ResourceID
		ns, name := provision.CRNamespace("euecs"), provision.CRName(resID)
		if _, err := cluster.Get(ns, name); err != nil {
			t.Fatalf("VM not created: %v", err)
		}

		// Restart: a fresh process (store + driver) over the same ledger and cluster.
		restarted, _ := newMultiStore()
		restarted.ledger, restarted.orderSrc = s.ledger, s.orderSrc
		restarted.driver = provision.NewVMDriverWithSpecs(cluster, nil, restarted.ledger)
		if rec := releaseAs(t, restarted, 100123, resID); rec.Code != 200 {
			t.Fatalf("release after restart: %d %s", rec.Code, rec.Body.String())
		}
		if _, err := cluster.Get(ns, name); !errors.Is(err, provision.ErrNotFound) {
			t.Fatalf("release after a restart left the VM running: %v", err)
		}
	})
}

// --- O8: release from failed, locked and expired states ---------------------------------

func TestReleaseFromFailedLockedExpired(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		// CREATE_FAILED: a rolled-back bucket; the release's Delete finds
		// nothing left and succeeds, and the bucket name was freed already.
		d := newScriptedDriver()
		s.driver = d
		failedO, retryO := paidIn(901, "euoss", "cn-north-1"), paidIn(902, "euoss", "cn-north-1")
		orders.add(100123, failedO)
		orders.add(200456, retryO)
		d.applyErr = errors.New("rc-storage: pool offline")
		if rec := fulfilAs(t, s, 100123, fulfilBodyFor(failedO, map[string]any{"bucket": "after-failure"})); rec.Code != 500 {
			t.Fatalf("failed provisioning: %d %s", rec.Code, rec.Body.String())
		}
		failedID := fulfilResourceID("euoss", "cn-north-1", 100123, 901)
		if got := mustGet(t, s.ledger, failedID); got.State != resource.StateCreateFailed {
			t.Fatalf("state = %s", got.State)
		}
		d.applyErr, d.next = nil, provision.PhaseReady
		if rec := fulfilAs(t, s, 200456, fulfilBodyFor(retryO, map[string]any{"bucket": "after-failure"})); rec.Code != 200 {
			t.Fatalf("a rolled-back bucket's name must be free: %d %s", rec.Code, rec.Body.String())
		}

		locked := putInstance(t, s.ledger, "euecs-cn-north-1-01-00000903", resource.ChargePostpaid,
			resource.StateRunning, resource.StateLocked)
		expired := putInstance(t, s.ledger, "euecs-cn-north-1-01-00000904", resource.ChargePrepaid,
			resource.StateRunning, resource.StateExpired)
		creating := putInstance(t, s.ledger, "euecs-cn-north-1-01-00000905", resource.ChargePostpaid)

		for _, id := range []string{failedID, locked.ResourceID, expired.ResourceID} {
			rec := releaseAs(t, s, 100123, id)
			if rec.Code != 200 {
				t.Fatalf("release %s: %d %s", id, rec.Code, rec.Body.String())
			}
			got := mustGet(t, s.ledger, id)
			if got.State != resource.StateReleased || got.ReleasedAt.IsZero() {
				t.Fatalf("%s after release = %s (released at %v)", id, got.State, got.ReleasedAt)
			}
			if _, deleted := d.counts(id); deleted == 0 {
				t.Fatalf("%s: release did not reclaim through the driver", id)
			}
		}
		wantCode(t, "release while CREATING", releaseAs(t, s, 100123, creating.ResourceID), 409, "Resource.StateTransitionFailed")
		wantCode(t, "release of another account's resource", releaseAs(t, s, 200456, locked.ResourceID), 404, "Resource.NotFound")
	})
}
