package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/starcloud/sc-platform/storage"
)

// SQLStore is the MySQL-backed Store over support_db.notification and
// notification_delivery. It is the production half of the port and the part
// that makes the trust-critical contract real: a release warning or an arrears
// notice that exists only in a process map is not evidence — "you never told
// me" is answered by a queryable row showing when the warning went out and
// through which channel (03§4.4.2).
type SQLStore struct {
	db *sql.DB
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore wires the store to support_db.
func NewSQLStore(_ context.Context, db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

// Save writes the notification and replaces its per-channel deliveries.
//
// The dispatcher calls Save at least twice per message (persist-before-send,
// then the outcome), so the write is an upsert by notification_id: the second
// Save updates status/attempts and refreshes the delivery rows. Deliveries are
// replaced wholesale rather than merged — the dispatcher owns their content,
// and a channel removed from the message's outcome must disappear.
func (s *SQLStore) Save(n Notification) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var sentAt any
		if !n.SentAt.IsZero() {
			sentAt = n.SentAt.UTC()
		}
		var lastErr any
		if n.LastError != "" {
			lastErr = n.LastError
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO notification
			   (notification_id, account_id, class, template_id, params_json, biz_key,
			    channels, status, attempts, last_error, sent_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
			    status = VALUES(status), attempts = VALUES(attempts),
			    last_error = VALUES(last_error), sent_at = VALUES(sent_at)`,
			n.NotificationID, n.AccountID, string(n.Class), n.TemplateID,
			marshalParams(n.Params), n.BizKey, joinChannels(n.Channels),
			string(n.Status), n.Attempts, lastErr, sentAt, n.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("notify: save %s: %w", n.NotificationID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM notification_delivery WHERE notification_id = ?`, n.NotificationID); err != nil {
			return fmt.Errorf("notify: clear deliveries for %s: %w", n.NotificationID, err)
		}
		for _, d := range n.Deliveries {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO notification_delivery
				   (notification_id, account_id, channel, success, provider, provider_message_id, error_msg, sent_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				n.NotificationID, n.AccountID, string(d.Channel), boolToInt(d.Success),
				nullableString(d.Provider), nullableString(d.MessageID), nullableString(d.Error),
				d.SentAt.UTC()); err != nil {
				return fmt.Errorf("notify: save delivery for %s: %w", n.NotificationID, err)
			}
		}
		return nil
	})
}

func (s *SQLStore) Get(notificationID string) (Notification, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	n, err := scanNotification(s.db.QueryRowContext(ctx,
		`SELECT notification_id, account_id, class, template_id, params_json, biz_key,
		        channels, status, attempts, last_error, sent_at, created_at
		   FROM notification WHERE notification_id = ?`, notificationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, err
	}
	if err := s.loadDeliveries(ctx, &n); err != nil {
		return Notification{}, err
	}
	return n, nil
}

// CountInWindow counts the notifications created for a tenant since `since`,
// powering the per-tenant throttle.
func (s *SQLStore) CountInWindow(accountID int64, since time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification WHERE account_id = ? AND created_at >= ?`,
		accountID, since.UTC()).Scan(&n); err != nil {
		return 0, fmt.Errorf("notify: count in window for account %d: %w", accountID, err)
	}
	return n, nil
}

// FindByBizKey returns the notifications recorded for a business object — the
// query a support agent runs when a customer says they were never warned.
func (s *SQLStore) FindByBizKey(accountID int64, class Class, bizKey string) ([]Notification, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT notification_id, account_id, class, template_id, params_json, biz_key,
		        channels, status, attempts, last_error, sent_at, created_at
		   FROM notification
		  WHERE account_id = ? AND class = ? AND biz_key = ?
		  ORDER BY created_at, notification_id`,
		accountID, string(class), bizKey)
	if err != nil {
		return nil, fmt.Errorf("notify: find by biz key %s: %w", bizKey, err)
	}
	defer rows.Close()

	out := make([]Notification, 0, 4)
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("notify: scan for biz key %s: %w", bizKey, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: find by biz key %s: %w", bizKey, err)
	}
	for i := range out {
		if err := s.loadDeliveries(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListByAccount returns the tenant's notification inbox, newest first. The
// inbox is a bounded view: a tenant's notification history grows forever by
// design (it is the evidence), but no console page needs more than the recent
// head of it.
func (s *SQLStore) ListByAccount(accountID int64) ([]Notification, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+notificationColumns+` FROM notification
		  WHERE account_id = ?
		  ORDER BY created_at DESC, notification_id DESC
		  LIMIT 100`, accountID)
	if err != nil {
		return nil, fmt.Errorf("notify: list for account %d: %w", accountID, err)
	}
	defer rows.Close()

	out := make([]Notification, 0, 16)
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("notify: scan for account %d: %w", accountID, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: list for account %d: %w", accountID, err)
	}
	for i := range out {
		if err := s.loadDeliveries(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *SQLStore) loadDeliveries(ctx context.Context, n *Notification) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, success, provider, provider_message_id, error_msg, sent_at
		   FROM notification_delivery WHERE notification_id = ? ORDER BY sent_at, channel`,
		n.NotificationID)
	if err != nil {
		return fmt.Errorf("notify: deliveries for %s: %w", n.NotificationID, err)
	}
	defer rows.Close()

	n.Deliveries = n.Deliveries[:0]
	for rows.Next() {
		var (
			d         Delivery
			success   int
			provider  sql.NullString
			messageID sql.NullString
			errMsg    sql.NullString
		)
		if err := rows.Scan(&d.Channel, &success, &provider, &messageID, &errMsg, &d.SentAt); err != nil {
			return fmt.Errorf("notify: scan delivery for %s: %w", n.NotificationID, err)
		}
		d.Success = success == 1
		d.Provider = provider.String
		d.MessageID = messageID.String
		d.Error = errMsg.String
		n.Deliveries = append(n.Deliveries, d)
	}
	return rows.Err()
}

const notificationColumns = `notification_id, account_id, class, template_id, params_json,
	biz_key, channels, status, attempts, last_error, sent_at, created_at`

func scanNotification(sc interface{ Scan(...any) error }) (Notification, error) {
	var (
		n        Notification
		params   []byte
		channels string
		lastErr  sql.NullString
		sentAt   sql.NullTime
	)
	if err := sc.Scan(&n.NotificationID, &n.AccountID, &n.Class, &n.TemplateID, &params,
		&n.BizKey, &channels, &n.Status, &n.Attempts, &lastErr, &sentAt, &n.CreatedAt); err != nil {
		return Notification{}, err
	}
	n.Params = unmarshalParams(params)
	n.Channels = splitChannels(channels)
	n.LastError = lastErr.String
	if sentAt.Valid {
		n.SentAt = sentAt.Time
	}
	return n, nil
}

// marshalParams keeps the JSON column valid for any parameter value: template
// params are arbitrary tenant strings, and a hand-rolled join would not escape
// quotes or backslashes.
func marshalParams(p map[string]string) string {
	if len(p) == 0 {
		return "{}"
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func unmarshalParams(b []byte) map[string]string {
	out := make(map[string]string, 4)
	_ = json.Unmarshal(b, &out)
	return out
}

func joinChannels(ch []Channel) string {
	parts := make([]string, 0, len(ch))
	for _, c := range ch {
		parts = append(parts, string(c))
	}
	return strings.Join(parts, ",")
}

func splitChannels(s string) []Channel {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]Channel, 0, len(parts))
	for _, p := range parts {
		out = append(out, Channel(strings.TrimSpace(p)))
	}
	return out
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
