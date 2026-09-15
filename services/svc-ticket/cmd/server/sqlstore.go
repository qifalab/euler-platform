package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/starcloud/sc-platform/storage"
)

// sqlStore is the MySQL-backed Store over support_db's ticket + ticket_message
// (03§4.4.4, account_id sharded).
//
// # What it changes relative to memoryStore
//
// Only the storage. The port's contract — Create/Get/ListByAccount/Reply/Close,
// "not found" and "closed" as typed errors, and the rule that a caller never
// receives a pointer into stored state — is the same one memoryStore implements,
// and the tests run against both.
//
// # Ids come from the 号段 allocator
//
// V1 declares ticket.ticket_id and ticket_message.message_id as BIGINT issued by
// the segment service (04§6.6); the phase-1 service minted UUIDs for both. A UUID
// cannot be written to a BIGINT column, so Create/Reply take ids from
// support_db.id_sequence and hand the caller the decimal string — the id stays an
// opaque string on the wire, which is what the DTO and the console consume.
//
// # Transitions lock the row, they do not race it
//
// memoryStore serialises Reply/Close under a mutex. The SQL equivalent is
// SELECT ... FOR UPDATE inside the transaction: read the current status and
// version, decide, then write with version = version + 1. Taking the lock first
// is what makes "append the message and move the status" atomic — the earlier
// phase of this service handed out the stored *Ticket and let handlers append to
// a shared slice outside any lock, which lost concurrent replies, and a SQL store
// that read the ticket and wrote it back without a lock would reintroduce exactly
// that failure one layer down.
type sqlStore struct {
	db         *sql.DB
	ticketIDs  func() int64
	messageIDs func() int64
}

var _ Store = (*sqlStore)(nil)

// statementTimeout bounds one store call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// newStore picks the backend. Persistence is opt-in (pkg-go/storage doc): with
// SC_DB_DSN set the tickets live in support_db, so a restart keeps them and the
// support channel stops losing in-flight threads; unset, the in-memory store
// keeps the demo and `go test` dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never touched —
// is a startup failure: a ticket channel that comes up "healthy" and then cannot
// open a ticket is worse than one that refuses to start.
func newStore(ctx context.Context) (Store, error) {
	db, ok, err := storage.MustOpenFor(ctx, "support_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemoryStore(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "support_db"); err != nil {
		return nil, err
	}
	return newSQLStore(ctx, db)
}

// persistentStore reports whether a store is backed by MySQL, for the startup
// log: an operator reading logs needs to know whether threads survive a restart.
func persistentStore(s Store) bool {
	_, ok := s.(*sqlStore)
	return ok
}

// newSQLStore wires the store to its id sequences. Both sequences must exist
// (support_db V2 seeds them): without them no ticket can be created at all.
func newSQLStore(ctx context.Context, db *sql.DB) (*sqlStore, error) {
	tickets, err := storage.OpenSequence(ctx, db, "ticket", 1000)
	if err != nil {
		return nil, err
	}
	messages, err := storage.OpenSequence(ctx, db, "ticket_message", 1000)
	if err != nil {
		return nil, err
	}
	return &sqlStore{db: db, ticketIDs: tickets.NextFunc(), messageIDs: messages.NextFunc()}, nil
}

// priorityCode maps the three-value phase-1 API onto the DDL's five levels
// (1紧急 2高 3中 4低). Code 2 is never written: the grading it stands for is the
// deferred 计划分级, and the API has no value between HIGH and NORMAL. It is
// folded into HIGH on read rather than dropped, so a row an operator graded by
// hand still surfaces as urgent.
func priorityCode(p string) (int, error) {
	switch p {
	case "HIGH":
		return 1, nil
	case "NORMAL":
		return 3, nil
	case "LOW":
		return 4, nil
	}
	return 0, fmt.Errorf("ticket: unknown priority %q", p)
}

func priorityFromCode(code int) string {
	switch code {
	case 1, 2:
		return "HIGH"
	case 3:
		return "NORMAL"
	default:
		return "LOW"
	}
}

// statusCode maps the DDL's lifecycle codes (1待受理 2处理中 3待用户确认 4已关闭
// 5已评价) onto the API's statuses. 已评价 sits past 已关闭 with no API value of
// its own, so it reads back as CLOSED — the state a caller acts on either way.
func statusCode(st TicketStatus) (int, error) {
	switch st {
	case StatusOpen:
		return 1, nil
	case StatusProcessing:
		return 2, nil
	case StatusWaitingReply:
		return 3, nil
	case StatusClosed:
		return 4, nil
	}
	return 0, fmt.Errorf("ticket: unknown status %q", st)
}

func statusFromCode(code int) TicketStatus {
	switch code {
	case 1:
		return StatusOpen
	case 2:
		return StatusProcessing
	case 3:
		return StatusWaitingReply
	default:
		return StatusClosed
	}
}

// ticketColumns is the ticket projection, in scanTicket's order.
const ticketColumns = `ticket_id, account_id, category, priority, status,
	sla_deadline, assignee, contact, client_token, version, created_at, updated_at`

// Create persists a new ticket and its first message.
//
// The id and the client token are assigned here: the caller passes a ticket with
// an id it minted for the in-memory store, and this store replaces both so the
// row's primary key and its idempotency key (uk_client_token) come from the
// allocator and the caller respectively.
func (s *sqlStore) Create(t *Ticket) error {
	priority, err := priorityCode(t.Priority)
	if err != nil {
		return err
	}
	status, err := statusCode(t.Status)
	if err != nil {
		return err
	}
	if t.ClientToken == "" {
		// The DDL requires one. A caller that did not supply a token gets a
		// server-minted one, which makes this call non-idempotent — the same
		// trade-off svc-order documents for its create path.
		t.ClientToken = fmt.Sprintf("ct-%d-%d", t.AccountID, time.Now().UnixNano())
	}
	t.TicketID = strconv.FormatInt(s.ticketIDs(), 10)
	t.Version = 0

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var sla any
		if !t.SLADeadline.IsZero() {
			sla = t.SLADeadline.UTC()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ticket
			   (ticket_id, account_id, category, priority, status, sla_deadline,
			    assignee, contact, client_token, version, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.TicketID, t.AccountID, t.Category, priority, status, sla,
			nullableString(t.Assignee), nullableString(t.Contact), t.ClientToken, t.Version,
			t.CreatedAt.UTC(), t.UpdatedAt.UTC()); err != nil {
			if storage.IsDuplicateKey(err) {
				// uk_client_token: a retried create. The caller sees it as a
				// duplicate id, which is what the memory store reports for the
				// same situation.
				return fmt.Errorf("ticket: duplicate ticket id %s", t.TicketID)
			}
			return fmt.Errorf("ticket: create %s: %w", t.TicketID, err)
		}
		for i := range t.Messages {
			if err := s.insertMessage(ctx, tx, t, &t.Messages[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// insertMessage appends one message row, assigning it a 号段 id.
func (s *sqlStore) insertMessage(ctx context.Context, tx *sql.Tx, t *Ticket, msg *Message) error {
	msg.ID = strconv.FormatInt(s.messageIDs(), 10)
	createdAt := msg.CreatedAt
	if createdAt.IsZero() {
		createdAt = t.CreatedAt
	}
	fromUser := 0
	if msg.FromUser {
		fromUser = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ticket_message
		   (message_id, ticket_id, account_id, sender, from_user, content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, t.TicketID, t.AccountID, msg.Author, fromUser, msg.Body, createdAt.UTC()); err != nil {
		return fmt.Errorf("ticket: append message to %s: %w", t.TicketID, err)
	}
	return nil
}

// Get returns the ticket with its thread, or errTicketNotFound when it does not
// exist or belongs to another account — the same single answer memoryStore
// gives, so a caller cannot probe for other tenants' ticket ids.
func (s *sqlStore) Get(accountID int64, ticketID string) (*Ticket, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	t, err := scanTicket(s.db.QueryRowContext(ctx,
		`SELECT `+ticketColumns+` FROM ticket WHERE ticket_id = ? AND account_id = ?`,
		ticketID, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errTicketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ticket: get %s: %w", ticketID, err)
	}
	if err := s.loadMessages(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// ListByAccount returns the account's tickets, newest first. The error return
// exists because a SQL backend has one; the memory store has nothing to report
// and returns nil.
func (s *sqlStore) ListByAccount(accountID int64) ([]*Ticket, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ticketColumns+`
		   FROM ticket
		  WHERE account_id = ?
		  ORDER BY created_at DESC, ticket_id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("ticket: list for account %d: %w", accountID, err)
	}
	defer rows.Close()

	tickets := make([]*Ticket, 0, 8)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("ticket: scan for account %d: %w", accountID, err)
		}
		tickets = append(tickets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ticket: list for account %d: %w", accountID, err)
	}
	// The console renders the thread's first message as the subject, so the
	// list carries each ticket's messages rather than making the UI N+1.
	for _, t := range tickets {
		if err := s.loadMessages(ctx, t); err != nil {
			return nil, err
		}
	}
	return tickets, nil
}

// Reply appends a message and moves the ticket to WAITING_REPLY.
func (s *sqlStore) Reply(accountID int64, ticketID string, msg Message, now time.Time) (*Ticket, error) {
	return s.transition(accountID, ticketID, now, func(ctx context.Context, tx *sql.Tx, t *Ticket) error {
		if t.Status == StatusClosed {
			return errTicketClosed
		}
		if err := s.insertMessage(ctx, tx, t, &msg); err != nil {
			return err
		}
		t.Messages = append(t.Messages, msg)
		return s.updateTicket(ctx, tx, t, StatusWaitingReply, now)
	})
}

// Close moves the ticket to CLOSED.
func (s *sqlStore) Close(accountID int64, ticketID string, now time.Time) (*Ticket, error) {
	return s.transition(accountID, ticketID, now, func(ctx context.Context, tx *sql.Tx, t *Ticket) error {
		if t.Status == StatusClosed {
			return errTicketClosed
		}
		return s.updateTicket(ctx, tx, t, StatusClosed, now)
	})
}

// transition runs a state change under the row lock: SELECT ... FOR UPDATE, the
// caller's mutation, then the guarded UPDATE and a fresh read. Holding the lock
// across the read and the write is what keeps two concurrent replies from both
// appending to a stale view of the thread.
func (s *sqlStore) transition(accountID int64, ticketID string, now time.Time,
	apply func(context.Context, *sql.Tx, *Ticket) error) (*Ticket, error) {

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		t, err := scanTicket(tx.QueryRowContext(ctx,
			`SELECT `+ticketColumns+` FROM ticket WHERE ticket_id = ? AND account_id = ? FOR UPDATE`,
			ticketID, accountID))
		if errors.Is(err, sql.ErrNoRows) {
			return errTicketNotFound
		}
		if err != nil {
			return fmt.Errorf("ticket: lock %s: %w", ticketID, err)
		}
		if err := s.loadMessagesTx(ctx, tx, t); err != nil {
			return err
		}
		if err := apply(ctx, tx, t); err != nil {
			return err
		}
		t.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Read the committed row back rather than returning the in-memory copy: the
	// caller gets what the database holds, including any column the UPDATE's
	// trigger or default touched.
	return s.Get(accountID, ticketID)
}

// updateTicket writes the status change under the optimistic lock and bumps the
// version. Zero rows matched would mean the row moved between the FOR UPDATE read
// and this write, which cannot happen inside the same transaction — so a
// mismatch is reported loudly instead of being retried away.
func (s *sqlStore) updateTicket(ctx context.Context, tx *sql.Tx, t *Ticket, next TicketStatus, now time.Time) error {
	status, err := statusCode(next)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE ticket
		    SET status = ?, version = ?, updated_at = ?
		  WHERE ticket_id = ? AND version = ?`,
		status, t.Version+1, now.UTC(), t.TicketID, t.Version)
	if err != nil {
		return fmt.Errorf("ticket: update %s: %w", t.TicketID, err)
	}
	if err := storage.Affected(res, nil); err != nil {
		return fmt.Errorf("ticket: update %s: %w", t.TicketID, err)
	}
	t.Status = next
	t.Version++
	return nil
}

// loadMessages fills a ticket's thread, oldest first.
func (s *sqlStore) loadMessages(ctx context.Context, t *Ticket) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT message_id, sender, from_user, content, created_at
		   FROM ticket_message
		  WHERE ticket_id = ?
		  ORDER BY created_at, message_id`, t.TicketID)
	if err != nil {
		return fmt.Errorf("ticket: load messages for %s: %w", t.TicketID, err)
	}
	defer rows.Close()
	return s.scanMessages(rows, t)
}

// loadMessagesTx is loadMessages inside a transaction (the row lock path).
func (s *sqlStore) loadMessagesTx(ctx context.Context, tx *sql.Tx, t *Ticket) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT message_id, sender, from_user, content, created_at
		   FROM ticket_message
		  WHERE ticket_id = ?
		  ORDER BY created_at, message_id`, t.TicketID)
	if err != nil {
		return fmt.Errorf("ticket: load messages for %s: %w", t.TicketID, err)
	}
	defer rows.Close()
	return s.scanMessages(rows, t)
}

func (s *sqlStore) scanMessages(rows *sql.Rows, t *Ticket) error {
	t.Messages = t.Messages[:0]
	for rows.Next() {
		var (
			msg      Message
			fromUser int
		)
		if err := rows.Scan(&msg.ID, &msg.Author, &fromUser, &msg.Body, &msg.CreatedAt); err != nil {
			return fmt.Errorf("ticket: scan message for %s: %w", t.TicketID, err)
		}
		msg.FromUser = fromUser == 1
		t.Messages = append(t.Messages, msg)
	}
	return rows.Err()
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTicket reads one ticket row, translating the DDL's codes back to the API's
// values and a NULL sla_deadline back to the zero time.
func scanTicket(sc rowScanner) (*Ticket, error) {
	var (
		t         Ticket
		priority  int
		status    int
		sla       sql.NullTime
		assignee  sql.NullString
		contact   sql.NullString
		clientTok string
	)
	if err := sc.Scan(&t.TicketID, &t.AccountID, &t.Category, &priority, &status, &sla,
		&assignee, &contact, &clientTok, &t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Priority = priorityFromCode(priority)
	t.Status = statusFromCode(status)
	t.Assignee = assignee.String
	t.Contact = contact.String
	t.ClientToken = clientTok
	if sla.Valid {
		t.SLADeadline = sla.Time
	}
	return &t, nil
}

// nullableString writes an empty string as SQL NULL, matching the DDL's
// nullable text columns.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
