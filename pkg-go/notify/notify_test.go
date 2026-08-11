package notify

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

const acct = int64(100123)

// memStore is an in-memory Store.
type memStore struct {
	mu    sync.Mutex
	items map[string]Notification
}

func newStore() *memStore { return &memStore{items: make(map[string]Notification)} }

func (m *memStore) Save(n Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[n.NotificationID] = n
	return nil
}

func (m *memStore) Get(id string) (Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.items[id]
	if !ok {
		return Notification{}, errors.New("not found")
	}
	return n, nil
}

func (m *memStore) CountInWindow(accountID int64, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, n := range m.items {
		if n.AccountID == accountID && n.Status != StatusSuppressed && !n.CreatedAt.Before(since) {
			count++
		}
	}
	return count, nil
}

func (m *memStore) FindByBizKey(accountID int64, class Class, bizKey string) ([]Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Notification
	for _, n := range m.items {
		if n.AccountID == accountID && n.Class == class && n.BizKey == bizKey {
			out = append(out, n)
		}
	}
	SortByTime(out)
	return out, nil
}

// mockSender records what it was asked to send and can be made to fail.
type mockSender struct {
	ch     Channel
	fail   bool
	sent   int
}

func (s *mockSender) Channel() Channel { return s.ch }

func (s *mockSender) Send(n Notification) (Delivery, error) {
	s.sent++
	if s.fail {
		return Delivery{Channel: s.ch, Success: false}, fmt.Errorf("%s gateway unavailable", s.ch)
	}
	return Delivery{
		Channel:   s.ch,
		Success:   true,
		Provider:  string(s.ch) + "-provider",
		MessageID: fmt.Sprintf("msg-%s-%d", s.ch, s.sent),
	}, nil
}

func newDispatcher(now time.Time) (*Dispatcher, *memStore, map[Channel]*mockSender) {
	store := newStore()
	var seq int64
	d := NewDispatcher(store, func() time.Time { return now }, func() string {
		seq++
		return fmt.Sprintf("ntf-%d", seq)
	})
	senders := map[Channel]*mockSender{
		ChannelInApp: {ch: ChannelInApp},
		ChannelSMS:   {ch: ChannelSMS},
		ChannelEmail: {ch: ChannelEmail},
	}
	for _, s := range senders {
		d.Register(s)
	}
	return d, store, senders
}

func releaseWarning() Notification {
	return Notification{
		AccountID:  acct,
		Class:      ClassReleaseWarning,
		TemplateID: "tpl-release-final",
		BizKey:     "scecs-cn-north-1-01-a1b2c3d4",
		Channels:   ChannelsFor(ClassReleaseWarning),
		Params:     map[string]string{"ResourceId": "scecs-cn-north-1-01-a1b2c3d4"},
	}
}

// --- Trust-critical classification ---

func TestTrustCriticalClasses(t *testing.T) {
	// 03§4.4.2 names exactly three.
	critical := []Class{ClassArrears, ClassExpiry, ClassReleaseWarning}
	for _, c := range critical {
		if !c.TrustCritical() {
			t.Errorf("%s must be trust-critical", c)
		}
	}
	for _, c := range []Class{ClassAlert, ClassOrder, ClassMarketing} {
		if c.TrustCritical() {
			t.Errorf("%s must not be trust-critical", c)
		}
	}
}

// --- Delivery evidence ---

func TestDeliveryIsRecordedAsEvidence(t *testing.T) {
	d, store, _ := newDispatcher(base)

	sent, err := d.Send(releaseWarning())
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != StatusSent {
		t.Fatalf("status = %s, want SENT", sent.Status)
	}
	if len(sent.Deliveries) != 3 {
		t.Fatalf("expected an attempt per channel, got %d", len(sent.Deliveries))
	}
	// Every delivery carries provider evidence for a dispute.
	for _, del := range sent.Deliveries {
		if del.Success && del.MessageID == "" {
			t.Errorf("%s delivery has no provider message id", del.Channel)
		}
	}

	// The record survives for the support query.
	stored, err := store.Get(sent.NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Delivered() {
		t.Fatal("stored record must show delivery")
	}
}

// TestVerifyDeliveredGatesRelease is the check the lifecycle runs before
// deleting data: it answers from persisted evidence, not optimism.
func TestVerifyDeliveredGatesRelease(t *testing.T) {
	d, _, _ := newDispatcher(base)
	const resourceID = "scecs-cn-north-1-01-a1b2c3d4"

	// Before any warning is sent, release must not be permitted.
	ok, err := d.VerifyDelivered(acct, ClassReleaseWarning, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("no warning was sent, so release must not be authorised")
	}

	if _, err := d.Send(releaseWarning()); err != nil {
		t.Fatal(err)
	}

	ok, err = d.VerifyDelivered(acct, ClassReleaseWarning, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a delivered warning should authorise release")
	}

	// A different resource is not covered by this warning.
	ok, _ = d.VerifyDelivered(acct, ClassReleaseWarning, "scecs-cn-north-1-01-ffffffff")
	if ok {
		t.Fatal("a warning about one resource must not authorise releasing another")
	}
}

// TestFailedWarningDoesNotAuthoriseRelease is the case that matters: if the
// notification could not be delivered, the destructive action must not proceed.
func TestFailedWarningDoesNotAuthoriseRelease(t *testing.T) {
	d, _, senders := newDispatcher(base)
	for _, s := range senders {
		s.fail = true
	}

	n, err := d.Send(releaseWarning())
	if !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("expected ErrNotDelivered, got %v", err)
	}
	if n.Status != StatusFailed {
		t.Fatalf("status = %s, want FAILED", n.Status)
	}

	ok, _ := d.VerifyDelivered(acct, ClassReleaseWarning, n.BizKey)
	if ok {
		t.Fatal("an undelivered warning must NOT authorise deleting the customer's data")
	}
}

// TestOneChannelSufficesForDelivery: a customer who got the SMS was warned,
// even if their email bounced. Requiring every channel would let a stale
// address block a release forever.
func TestOneChannelSufficesForDelivery(t *testing.T) {
	d, _, senders := newDispatcher(base)
	senders[ChannelEmail].fail = true
	senders[ChannelInApp].fail = true
	// SMS still works.

	n, err := d.Send(releaseWarning())
	if err != nil {
		t.Fatalf("one working channel should be enough: %v", err)
	}
	if !n.Delivered() || n.Status != StatusSent {
		t.Fatalf("status = %s, delivered = %v", n.Status, n.Delivered())
	}
}

// --- Rate limiting ---

// TestThrottlingProtectsAttention: an alert storm must not bury the messages
// that matter.
func TestThrottlingSuppressesOrdinaryNotifications(t *testing.T) {
	d, _, _ := newDispatcher(base)

	for i := 0; i < PerTenantRateLimit; i++ {
		if _, err := d.Send(Notification{
			AccountID: acct, Class: ClassAlert, TemplateID: "tpl-alert",
			BizKey: fmt.Sprintf("alert-%d", i), Channels: ChannelsFor(ClassAlert),
		}); err != nil {
			t.Fatalf("alert %d should have been sent: %v", i, err)
		}
	}

	_, err := d.Send(Notification{
		AccountID: acct, Class: ClassAlert, TemplateID: "tpl-alert",
		BizKey: "alert-overflow", Channels: ChannelsFor(ClassAlert),
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited past the ceiling, got %v", err)
	}
}

// TestTrustCriticalBypassesThrottling is the crucial exemption: throttling an
// arrears warning to protect the inbox would defeat the point of sending it.
func TestTrustCriticalBypassesThrottling(t *testing.T) {
	d, _, _ := newDispatcher(base)

	// Saturate the tenant's budget with alerts.
	for i := 0; i < PerTenantRateLimit*2; i++ {
		_, _ = d.Send(Notification{
			AccountID: acct, Class: ClassAlert, TemplateID: "tpl-alert",
			BizKey: fmt.Sprintf("alert-%d", i), Channels: ChannelsFor(ClassAlert),
		})
	}

	// The arrears warning still goes out.
	n, err := d.Send(Notification{
		AccountID: acct, Class: ClassArrears, TemplateID: "tpl-arrears",
		BizKey: "account-100123", Channels: ChannelsFor(ClassArrears),
	})
	if err != nil {
		t.Fatalf("an arrears warning must never be throttled: %v", err)
	}
	if n.Status != StatusSent {
		t.Fatalf("status = %s, want SENT", n.Status)
	}

	// So does the release warning.
	if _, err := d.Send(releaseWarning()); err != nil {
		t.Fatalf("a release warning must never be throttled: %v", err)
	}
}

// --- Opt-out ---

// TestOnlyMarketingIsOptional: a customer cannot opt out of being told their
// data is about to be deleted.
func TestOnlyMarketingIsOptional(t *testing.T) {
	if !OptOutAllowed(ClassMarketing) {
		t.Error("marketing must be opt-out-able")
	}
	for _, c := range []Class{ClassArrears, ClassExpiry, ClassReleaseWarning, ClassOrder, ClassAlert} {
		if OptOutAllowed(c) {
			t.Errorf("%s must not be opt-out-able", c)
		}
	}
}

// --- Channel fan-out ---

func TestReleaseWarningFansOutWidely(t *testing.T) {
	// The last warning before deletion goes everywhere: a single channel is a
	// single point of failure for a message whose non-delivery destroys data.
	channels := ChannelsFor(ClassReleaseWarning)
	if len(channels) < 3 {
		t.Fatalf("release warning uses %d channels, want at least 3", len(channels))
	}
	has := func(c Channel) bool {
		for _, x := range channels {
			if x == c {
				return true
			}
		}
		return false
	}
	if !has(ChannelSMS) || !has(ChannelEmail) || !has(ChannelInApp) {
		t.Fatalf("release warning should reach in-app, SMS and email; got %v", channels)
	}
}

// --- Validation ---

func TestBizKeyRequiredForTrustCritical(t *testing.T) {
	// Without a business key the record cannot be produced as evidence later,
	// which defeats the reason it is persisted.
	d, _, _ := newDispatcher(base)
	n := releaseWarning()
	n.BizKey = ""
	if _, err := d.Send(n); !errors.Is(err, ErrMissingBizKey) {
		t.Fatalf("expected ErrMissingBizKey, got %v", err)
	}
}

func TestAccountRequired(t *testing.T) {
	d, _, _ := newDispatcher(base)
	n := releaseWarning()
	n.AccountID = 0
	if _, err := d.Send(n); !errors.Is(err, ErrMissingAccount) {
		t.Fatalf("expected ErrMissingAccount, got %v", err)
	}
}

func TestChannelsRequired(t *testing.T) {
	d, _, _ := newDispatcher(base)
	n := releaseWarning()
	n.Channels = nil
	if _, err := d.Send(n); !errors.Is(err, ErrNoChannels) {
		t.Fatalf("expected ErrNoChannels, got %v", err)
	}
}

func TestUnregisteredChannelRecordedAsFailure(t *testing.T) {
	// A misconfigured channel must show up in the record rather than silently
	// reducing the fan-out.
	d, _, _ := newDispatcher(base)
	n := releaseWarning()
	n.Channels = []Channel{ChannelWebhook} // no sender registered

	_, err := d.Send(n)
	if !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("expected ErrNotDelivered, got %v", err)
	}
}

// --- Retry and escalation ---

func TestRetryBackoff(t *testing.T) {
	n := Notification{Status: StatusFailed, Attempts: 1, CreatedAt: base}

	if RetryDue(n, base.Add(30*time.Second)) {
		t.Error("should not retry before the first backoff elapses")
	}
	if !RetryDue(n, base.Add(2*time.Minute)) {
		t.Error("should retry after the backoff elapses")
	}

	// A successful message is never retried.
	sentMsg := Notification{Status: StatusSent, Attempts: 1, CreatedAt: base}
	if RetryDue(sentMsg, base.Add(time.Hour)) {
		t.Error("a delivered message must not be retried")
	}
}

// TestExhaustedEscalates: silently giving up on a release warning is the one
// failure this package exists to prevent.
func TestExhaustedEscalates(t *testing.T) {
	n := Notification{Status: StatusFailed, Attempts: MaxDeliveryAttempts, CreatedAt: base}
	if !Exhausted(n) {
		t.Fatal("a message past its attempt budget must escalate")
	}
	if RetryDue(n, base.Add(time.Hour)) {
		t.Fatal("an exhausted message must stop retrying and hand over to a human")
	}

	fewer := Notification{Status: StatusFailed, Attempts: 1, CreatedAt: base}
	if Exhausted(fewer) {
		t.Fatal("one failure is not exhaustion")
	}
}

// --- Multiple warnings for one resource ---

func TestAnyDeliveredWarningAuthorisesRelease(t *testing.T) {
	// The platform may warn several times (7/3/1 days out). One delivered
	// warning is enough evidence, even if a later attempt failed.
	d, _, senders := newDispatcher(base)
	const resourceID = "scecs-cn-north-1-01-a1b2c3d4"

	// First warning succeeds.
	if _, err := d.Send(releaseWarning()); err != nil {
		t.Fatal(err)
	}
	// A later attempt fails entirely.
	for _, s := range senders {
		s.fail = true
	}
	_, _ = d.Send(releaseWarning())

	ok, err := d.VerifyDelivered(acct, ClassReleaseWarning, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("an earlier successful warning still counts as evidence")
	}
}

func TestVerifyRejectsNonCriticalClass(t *testing.T) {
	d, _, _ := newDispatcher(base)
	if _, err := d.VerifyDelivered(acct, ClassMarketing, "x"); err == nil {
		t.Fatal("delivery verification only applies to trust-critical classes")
	}
}
