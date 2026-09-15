// Package notify implements the notification pipeline
// (03-backend-services.md §4.4.2).
//
// # Three classes are a trust boundary, not a feature
//
// 03§4.4.2 singles out three notification classes that MUST be persisted and
// queryable: 欠费催收, 到期提醒, 释放预告. 对标启示 8 explains why — the
// lifecycle state machine and its notifications are the floor of commercial
// credibility. When a customer's data is deleted, the only acceptable answer
// to "you never told me" is a queryable record showing when the warning was
// sent and through which channel.
//
// So these three classes are never fire-and-forget: they are persisted before
// dispatch, their delivery outcome is recorded, and a failure to deliver blocks
// the destructive action rather than being logged and forgotten.
//
// # Rate limiting protects the customer, not the platform
//
// A resource storm can generate thousands of alerts in a minute. Per-tenant
// throttling (10/minute, 05§8.3) exists so the one notification that matters
// is not buried under nine hundred that do not — a customer who mutes the
// platform after an alert flood will also miss the arrears warning.
//
// Critically, the three trust-critical classes are EXEMPT from suppression:
// throttling a release warning to protect a customer's inbox would defeat the
// purpose of sending it.
package notify

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Class categorises a notification. The three trust-critical classes are
// distinguished from ordinary ones because they carry different delivery
// guarantees.
type Class string

const (
	// ClassArrears 欠费催收 — sent during the grace window, every
	// DunningInterval, before service stops.
	ClassArrears Class = "ARREARS"
	// ClassExpiry 到期提醒 — 30/15/7/3/1 days before a subscription lapses.
	ClassExpiry Class = "EXPIRY"
	// ClassReleaseWarning 释放预告 — the final warning before data deletion.
	// This is the most consequential message the platform ever sends.
	ClassReleaseWarning Class = "RELEASE_WARNING"

	// ClassAlert is a monitoring alert (customer-configured rules).
	ClassAlert Class = "ALERT"
	// ClassOrder is an order/fulfilment status update.
	ClassOrder Class = "ORDER"
	// ClassMarketing is promotional. Opt-out applies here and nowhere else.
	ClassMarketing Class = "MARKETING"
)

// TrustCritical reports whether this class must be persisted, provably
// delivered, and exempt from throttling and opt-out (03§4.4.2).
func (c Class) TrustCritical() bool {
	return c == ClassArrears || c == ClassExpiry || c == ClassReleaseWarning
}

// Channel is a delivery mechanism.
type Channel string

const (
	ChannelInApp   Channel = "IN_APP"
	ChannelSMS     Channel = "SMS"
	ChannelEmail   Channel = "EMAIL"
	ChannelWebhook Channel = "WEBHOOK"
)

// Status is the delivery state of a notification.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusSent      Status = "SENT"
	StatusFailed    Status = "FAILED"
	StatusSuppressed Status = "SUPPRESSED" // throttled; never applies to trust-critical
)

// PerTenantRateLimit is the notification ceiling per tenant per minute
// (05§8.3). It protects the customer's attention, not the platform's capacity.
const PerTenantRateLimit = 10

// RateWindow is the throttling window.
const RateWindow = time.Minute

// MaxDeliveryAttempts bounds retries for a trust-critical message before it
// escalates to a human. Silently giving up on a release warning is the one
// failure mode this package exists to prevent.
const MaxDeliveryAttempts = 3

// Notification is one message to one tenant.
type Notification struct {
	NotificationID string
	AccountID      int64 // shard key
	Class          Class
	TemplateID     string
	Params         map[string]string
	Channels       []Channel
	// BizKey ties the message to what caused it: a resource id for a release
	// warning, an account for arrears. Without it a customer asking "what is
	// this about" cannot be answered.
	BizKey    string
	Status    Status
	Attempts  int
	CreatedAt time.Time
	SentAt    time.Time
	// Deliveries records the per-channel outcome — the evidence that the
	// warning was actually sent.
	Deliveries []Delivery
	LastError  string
}

// Delivery is the outcome for one channel.
type Delivery struct {
	Channel   Channel
	Success   bool
	SentAt    time.Time
	Provider  string // SMS gateway, mail relay, etc.
	MessageID string // provider-side id, for dispute evidence
	Error     string
}

// Delivered reports whether at least one channel succeeded.
//
// One channel is enough: a customer who received the SMS was warned, even if
// the email bounced. Requiring all channels would let a stale email address
// block a release indefinitely.
func (n Notification) Delivered() bool {
	for _, d := range n.Deliveries {
		if d.Success {
			return true
		}
	}
	return false
}

// Errors.
var (
	ErrNoChannels       = errors.New("notify: no delivery channels")
	ErrRateLimited      = errors.New("notify: tenant rate limit exceeded")
	ErrNotDelivered     = errors.New("notify: message was not delivered on any channel")
	ErrMissingAccount   = errors.New("notify: no account attribution")
	ErrMissingBizKey    = errors.New("notify: trust-critical notification requires a BizKey")
	ErrDeliveryExhausted = errors.New("notify: delivery attempts exhausted, escalating")
	ErrNotFound         = errors.New("notify: notification not found")
)

// Sender delivers a message on one channel. Production implementations wrap
// the SMS gateway, mail relay and in-app store.
type Sender interface {
	Channel() Channel
	Send(n Notification) (Delivery, error)
}

// Store persists notifications and their delivery outcomes.
type Store interface {
	Save(n Notification) error
	Get(notificationID string) (Notification, error)
	// CountInWindow returns how many notifications were sent to a tenant since
	// the given time, for throttling.
	CountInWindow(accountID int64, since time.Time) (int, error)
	// FindByBizKey returns the notifications recorded for a business object —
	// the query a support agent runs when a customer says they were never
	// warned.
	FindByBizKey(accountID int64, class Class, bizKey string) ([]Notification, error)
	// ListByAccount returns the tenant's notification inbox, newest first —
	// the console view on top of the same rows.
	ListByAccount(accountID int64) ([]Notification, error)
}

// Dispatcher sends notifications and records the outcome.
type Dispatcher struct {
	store   Store
	senders map[Channel]Sender
	now     func() time.Time
	nextID  func() string
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(store Store, now func() time.Time, nextID func() string) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	if nextID == nil {
		var seq int64
		nextID = func() string { seq++; return fmt.Sprintf("ntf-%d", seq) }
	}
	return &Dispatcher{
		store:   store,
		senders: make(map[Channel]Sender),
		now:     now,
		nextID:  nextID,
	}
}

// Register adds a channel sender.
func (d *Dispatcher) Register(s Sender) { d.senders[s.Channel()] = s }

// Send dispatches a notification, persisting it before and after delivery.
//
// Persist-before-send is deliberate: a message that crashes mid-delivery is
// still on record as attempted, which is what lets the retry sweeper find it
// and what lets a support agent see that the platform tried.
func (d *Dispatcher) Send(n Notification) (Notification, error) {
	if n.AccountID == 0 {
		return Notification{}, ErrMissingAccount
	}
	if len(n.Channels) == 0 {
		return Notification{}, ErrNoChannels
	}
	// A trust-critical message with no business key cannot be produced as
	// evidence later, which defeats the reason it is persisted at all.
	if n.Class.TrustCritical() && n.BizKey == "" {
		return Notification{}, ErrMissingBizKey
	}

	now := d.now()
	if n.NotificationID == "" {
		n.NotificationID = d.nextID()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}

	// Throttling protects the customer's attention — but never at the cost of
	// a warning they need. Trust-critical classes bypass it entirely.
	if !n.Class.TrustCritical() {
		count, err := d.store.CountInWindow(n.AccountID, now.Add(-RateWindow))
		if err != nil {
			return Notification{}, err
		}
		if count >= PerTenantRateLimit {
			n.Status = StatusSuppressed
			if err := d.store.Save(n); err != nil {
				return Notification{}, err
			}
			return n, ErrRateLimited
		}
	}

	n.Status = StatusPending
	n.Attempts++
	if err := d.store.Save(n); err != nil {
		return Notification{}, err
	}

	// Attempt every configured channel. One success is enough, but all are
	// tried so the record shows what was reachable.
	for _, ch := range n.Channels {
		sender, ok := d.senders[ch]
		if !ok {
			n.Deliveries = append(n.Deliveries, Delivery{
				Channel: ch, Success: false, SentAt: now,
				Error: "no sender registered for channel",
			})
			continue
		}
		del, err := sender.Send(n)
		del.Channel = ch
		if del.SentAt.IsZero() {
			del.SentAt = now
		}
		if err != nil {
			del.Success = false
			del.Error = err.Error()
			n.LastError = err.Error()
		}
		n.Deliveries = append(n.Deliveries, del)
	}

	if n.Delivered() {
		n.Status = StatusSent
		n.SentAt = now
	} else {
		n.Status = StatusFailed
	}
	if err := d.store.Save(n); err != nil {
		return Notification{}, err
	}
	if n.Status == StatusFailed {
		return n, ErrNotDelivered
	}
	return n, nil
}

// VerifyDelivered checks that a trust-critical notification was actually
// delivered for a business object.
//
// This is the gate the resource lifecycle calls before releasing data
// (03§5.4). It answers, from persisted evidence rather than from optimism,
// whether the customer was warned.
func (d *Dispatcher) VerifyDelivered(accountID int64, class Class, bizKey string) (bool, error) {
	if !class.TrustCritical() {
		return false, fmt.Errorf("notify: %s is not a trust-critical class", class)
	}
	records, err := d.store.FindByBizKey(accountID, class, bizKey)
	if err != nil {
		return false, err
	}
	for _, r := range records {
		if r.Delivered() {
			return true, nil
		}
	}
	return false, nil
}

// Exhausted reports whether a message has used all its delivery attempts and
// must escalate to a human.
//
// Silently giving up on a release warning is precisely the failure this
// package exists to prevent: the destructive action must not proceed on the
// assumption that the notification "probably" went out.
func Exhausted(n Notification) bool {
	return n.Status == StatusFailed && n.Attempts >= MaxDeliveryAttempts
}

// RetryDue reports whether a failed notification should be retried now, with
// exponential backoff.
func RetryDue(n Notification, now time.Time) bool {
	if n.Status != StatusFailed || Exhausted(n) {
		return false
	}
	backoff := time.Duration(1<<uint(n.Attempts-1)) * time.Minute
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	return !now.Before(n.CreatedAt.Add(backoff))
}

// ChannelsFor returns the default channels for a class.
//
// The trust-critical classes fan out widely on purpose: a single channel is a
// single point of failure for a message whose non-delivery destroys data
// (01§5.4 D8 specifies 站内信+短信+邮件 for renewal reminders).
func ChannelsFor(c Class) []Channel {
	switch c {
	case ClassReleaseWarning:
		// The last warning before deletion goes everywhere available.
		return []Channel{ChannelInApp, ChannelSMS, ChannelEmail}
	case ClassArrears:
		return []Channel{ChannelInApp, ChannelSMS}
	case ClassExpiry:
		return []Channel{ChannelInApp, ChannelSMS, ChannelEmail}
	case ClassAlert:
		return []Channel{ChannelInApp, ChannelWebhook}
	case ClassOrder:
		return []Channel{ChannelInApp}
	case ClassMarketing:
		return []Channel{ChannelEmail}
	default:
		return []Channel{ChannelInApp}
	}
}

// OptOutAllowed reports whether a tenant may unsubscribe from a class.
//
// Only marketing is optional. A customer cannot opt out of being told their
// data is about to be deleted — that is not a preference the platform can
// honour and still claim to have warned them.
func OptOutAllowed(c Class) bool {
	return c == ClassMarketing
}

// SortByTime orders notifications chronologically for a support view.
func SortByTime(ns []Notification) {
	sort.SliceStable(ns, func(i, j int) bool {
		return ns[i].CreatedAt.Before(ns[j].CreatedAt)
	})
}
