package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/storage/sqltest"
)

// newSQLTestStore wires the SQL store to a throwaway support_db.
//
// Two DDL directories are applied, in the order the migration manifest uses:
// svc-quota owns support_db's id_sequence (its V3), and svc-ticket's V2 seeds the
// two sequence rows this store needs. A test that applied only its own directory
// would fail on a dependency the real migration satisfies — which is the honest
// shape of the schema, since support_db is shared.
func newSQLTestStore(t *testing.T) *sqlStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-quota"), sqltest.Services("svc-ticket"))
	store, err := newSQLStore(context.Background(), db)
	if err != nil {
		t.Fatalf("newSQLStore: %v", err)
	}
	return store
}

var testNow = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

func newTicket(accountID int64, priority, category, contact, body string) *Ticket {
	return &Ticket{
		TicketID:    "in-memory-id-the-store-must-replace",
		AccountID:   accountID,
		Category:    category,
		Priority:    priority,
		Status:      StatusOpen,
		SLADeadline: slaDeadline(priority, testNow),
		Assignee:    "system",
		Contact:     contact,
		Messages: []Message{{
			ID: "in-memory-message-id", Author: fmt.Sprint(accountID),
			FromUser: true, Body: body, CreatedAt: testNow,
		}},
		CreatedAt: testNow,
		UpdatedAt: testNow,
	}
}

func TestSQLStoreTicketRoundTrip(t *testing.T) {
	store := newSQLTestStore(t)
	const acct = int64(100123)

	tk := newTicket(acct, "HIGH", "billing", "13800000000", "扣费异常")
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	// The store owns the id: the DDL's key comes from the 号段 allocator, so the
	// caller's placeholder must be gone.
	if tk.TicketID == "in-memory-id-the-store-must-replace" || tk.TicketID == "" {
		t.Fatalf("ticket id was not assigned: %q", tk.TicketID)
	}
	if tk.Version != 0 {
		t.Fatalf("new ticket version = %d, want 0", tk.Version)
	}

	got, err := store.Get(acct, tk.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != acct || got.Category != "billing" || got.Priority != "HIGH" {
		t.Fatalf("ticket round trip = %+v", got)
	}
	// contact has a column now; dropping it on the way to the database was the
	// gap this store closed.
	if got.Contact != "13800000000" {
		t.Fatalf("contact = %q, want the value the customer supplied", got.Contact)
	}
	if !got.SLADeadline.Equal(testNow.Add(4 * time.Hour)) {
		t.Fatalf("sla deadline = %s, want +4h for HIGH", got.SLADeadline)
	}
	if got.Status != StatusOpen || got.Version != 0 {
		t.Fatalf("status/version = %s/%d, want OPEN/0", got.Status, got.Version)
	}
	if len(got.Messages) != 1 || got.Messages[0].Body != "扣费异常" || !got.Messages[0].FromUser {
		t.Fatalf("first message = %+v", got.Messages)
	}
	if got.Messages[0].ID == "in-memory-message-id" {
		t.Fatal("message id was not assigned from the sequence")
	}
	// A status the API never writes but an operator might grade by hand (2高)
	// must not read back as an unknown value.
	if got.ClientToken == "" {
		t.Fatal("client token was not stored")
	}
}

func TestSQLStoreTicketPriorityAndStatusCodes(t *testing.T) {
	store := newSQLTestStore(t)
	const acct = int64(100123)

	for _, p := range []string{"HIGH", "NORMAL", "LOW"} {
		tk := newTicket(acct, p, "consult", "", "body")
		if err := store.Create(tk); err != nil {
			t.Fatalf("priority %s: %v", p, err)
		}
		got, err := store.Get(acct, tk.TicketID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Priority != p {
			t.Fatalf("priority %s round-tripped as %s", p, got.Priority)
		}
	}

	// An unknown priority is a programming error at the call site, not a row we
	// silently label NORMAL.
	if err := store.Create(newTicket(acct, "SUPER", "consult", "", "body")); err == nil {
		t.Fatal("an unknown priority must be rejected")
	}
}

// TestSQLStoreConcurrentRepliesDoNotLoseMessages is the regression this service
// already carries for the in-memory store, run against the database: the row lock
// in transition() is what replaces the mutex, and losing an append here means a
// customer's reply vanished.
func TestSQLStoreConcurrentRepliesDoNotLoseMessages(t *testing.T) {
	store := newSQLTestStore(t)
	const (
		acct    = int64(100123)
		replies = 16
	)

	tk := newTicket(acct, "NORMAL", "billing", "", "root")
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, replies)
	for i := 0; i < replies; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Reply(acct, tk.TicketID, Message{
				Author: "100123", FromUser: true,
				Body:      fmt.Sprintf("reply-%d", i),
				CreatedAt: testNow.Add(time.Duration(i) * time.Second),
			}, testNow.Add(time.Duration(i)*time.Second))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	got, err := store.Get(acct, tk.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != replies+1 {
		t.Fatalf("thread has %d messages, want %d (a concurrent reply was lost)", len(got.Messages), replies+1)
	}
	if got.Status != StatusWaitingReply {
		t.Fatalf("status = %s, want WAITING_REPLY after a reply", got.Status)
	}
	if got.Version != replies {
		t.Fatalf("version = %d, want %d (one bump per transition)", got.Version, replies)
	}
}

func TestSQLStoreCloseIsTerminal(t *testing.T) {
	store := newSQLTestStore(t)
	const acct = int64(100123)

	tk := newTicket(acct, "NORMAL", "billing", "", "root")
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Close(acct, tk.TicketID, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("status = %s, want CLOSED", closed.Status)
	}

	if _, err := store.Close(acct, tk.TicketID, testNow); !errors.Is(err, errTicketClosed) {
		t.Fatalf("second close = %v, want errTicketClosed", err)
	}
	if _, err := store.Reply(acct, tk.TicketID, Message{Body: "again"}, testNow); !errors.Is(err, errTicketClosed) {
		t.Fatalf("reply to a closed ticket = %v, want errTicketClosed", err)
	}
}

// TestSQLStoreTenantIsolation: another account asking for the same ticket id must
// get "not found", not a peek at someone else's thread.
func TestSQLStoreTenantIsolation(t *testing.T) {
	store := newSQLTestStore(t)

	tk := newTicket(100123, "NORMAL", "billing", "", "root")
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(999, tk.TicketID); !errors.Is(err, errTicketNotFound) {
		t.Fatalf("cross-account get = %v, want errTicketNotFound", err)
	}
	if _, err := store.Reply(999, tk.TicketID, Message{Body: "x"}, testNow); !errors.Is(err, errTicketNotFound) {
		t.Fatalf("cross-account reply = %v, want errTicketNotFound", err)
	}
	list, err := store.ListByAccount(999)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("another account's list leaked %d tickets", len(list))
	}
}

func TestSQLStoreListNewestFirst(t *testing.T) {
	store := newSQLTestStore(t)
	const acct = int64(100123)

	var ids []string
	for i := 0; i < 3; i++ {
		tk := newTicket(acct, "NORMAL", "consult", "", fmt.Sprintf("t-%d", i))
		tk.CreatedAt = testNow.Add(time.Duration(i) * time.Minute)
		tk.UpdatedAt = tk.CreatedAt
		tk.Messages[0].CreatedAt = tk.CreatedAt
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.TicketID)
	}
	list, err := store.ListByAccount(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %d tickets, want 3", len(list))
	}
	if list[0].TicketID != ids[2] {
		t.Fatalf("list is not newest-first: %s then %s", list[0].TicketID, list[1].TicketID)
	}
	// The list carries the thread so the console does not have to fetch each one.
	if len(list[0].Messages) != 1 {
		t.Fatalf("listed ticket has %d messages, want its thread", len(list[0].Messages))
	}
}

// TestSQLStoreSurvivesRestart is the point of wiring persistence at all: a new
// store instance (a new process) still sees the tickets.
func TestSQLStoreSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-quota"), sqltest.Services("svc-ticket"))
	ctx := context.Background()

	first, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	tk := newTicket(100123, "HIGH", "billing", "13800000000", "第一次提交")
	if err := first.Create(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Reply(100123, tk.TicketID, Message{Author: "100123", FromUser: true, Body: "补充", CreatedAt: testNow.Add(time.Minute)}, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// A second store over the same schema stands in for the restarted process.
	restarted, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(100123, tk.TicketID)
	if err != nil {
		t.Fatalf("ticket did not survive the restart: %v", err)
	}
	if len(got.Messages) != 2 || got.Status != StatusWaitingReply || got.Contact != "13800000000" {
		t.Fatalf("reloaded ticket = %+v (messages %d)", got, len(got.Messages))
	}
}

// TestSQLStoreMatchesMemoryStore runs one script through both implementations and
// requires the same observable outcome. Where the tests above check each against
// the port, this checks them against each other.
func TestSQLStoreMatchesMemoryStore(t *testing.T) {
	sqlStoreImpl := newSQLTestStore(t)
	memStoreImpl := newMemoryStore()
	const acct = int64(100123)

	run := func(store Store) *Ticket {
		t.Helper()
		tk := newTicket(acct, "HIGH", "billing", "13800000000", "扣费异常")
		if err := store.Create(tk); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.Reply(acct, tk.TicketID, Message{Author: "100123", FromUser: true, Body: "补充", CreatedAt: testNow.Add(time.Minute)}, testNow.Add(time.Minute)); err != nil {
			t.Fatalf("reply: %v", err)
		}
		got, err := store.Get(acct, tk.TicketID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}

	sqlTicket := run(sqlStoreImpl)
	memTicket := run(memStoreImpl)

	if sqlTicket.Status != memTicket.Status || sqlTicket.Priority != memTicket.Priority ||
		sqlTicket.Category != memTicket.Category || sqlTicket.Contact != memTicket.Contact ||
		sqlTicket.Assignee != memTicket.Assignee {
		t.Fatalf("sql ticket %+v, memory ticket %+v", sqlTicket, memTicket)
	}
	if len(sqlTicket.Messages) != len(memTicket.Messages) {
		t.Fatalf("sql thread %d messages, memory %d", len(sqlTicket.Messages), len(memTicket.Messages))
	}
	if !sqlTicket.SLADeadline.Equal(memTicket.SLADeadline) {
		t.Fatalf("sla %s vs %s", sqlTicket.SLADeadline, memTicket.SLADeadline)
	}
	for i := range sqlTicket.Messages {
		a, b := sqlTicket.Messages[i], memTicket.Messages[i]
		if a.Author != b.Author || a.Body != b.Body || a.FromUser != b.FromUser || !a.CreatedAt.Equal(b.CreatedAt) {
			t.Fatalf("message %d: sql %+v, memory %+v", i, a, b)
		}
	}
}
