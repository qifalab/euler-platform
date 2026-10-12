package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMainPathApp builds the app the way main() does, persistence off.
func newMainPathApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("EULER_DB_DSN", "")
	store, err := newStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return newApp(store)
}

func apiCreate(t *testing.T, a *app, body string) (*httptest.ResponseRecorder, ticketDTO) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets", strings.NewReader(body))
	req.Header.Set("X-Euler-Account-Id", "100123")
	rec := httptest.NewRecorder()
	a.handleAPICreate(rec, req)
	var env struct {
		Data ticketDTO `json:"Data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec, env.Data
}

// Regression: with both title and message supplied, the title was discarded;
// the DTO's only "title" was the first message.
func TestAPICreateStoresTitleSeparately(t *testing.T) {
	a := newMainPathApp(t)
	rec, dto := apiCreate(t, a, `{"category":"故障","priority":"HIGH","title":"无法登录控制台","message":"登录时提示密码错误","contact":"13800000000"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if dto.Title != "无法登录控制台" || dto.Message != "登录时提示密码错误" || dto.Contact != "13800000000" {
		t.Fatalf("created DTO = %+v", dto)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tickets", nil)
	req.Header.Set("X-Euler-Account-Id", "100123")
	lrec := httptest.NewRecorder()
	a.handleAPIList(lrec, req)
	var list struct {
		Data struct {
			Tickets []map[string]any `json:"tickets"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data.Tickets) != 1 || list.Data.Tickets[0]["title"] != "无法登录控制台" {
		t.Fatalf("listed tickets = %+v, want JSON title", list.Data.Tickets)
	}
}

func TestAPICreateTitleOnlyAndOverlongTitle(t *testing.T) {
	a := newMainPathApp(t)
	rec, dto := apiCreate(t, a, `{"category":"咨询","title":"发票怎么开"}`)
	if rec.Code != http.StatusCreated || dto.Title != "发票怎么开" || dto.Message != "发票怎么开" {
		t.Fatalf("title-only create: %d %+v", rec.Code, dto)
	}
	rec, _ = apiCreate(t, a, `{"category":"咨询","title":"`+strings.Repeat("长", maxTitleRunes+1)+`","message":"x"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Ticket.InvalidTitle") {
		t.Fatalf("overlong title: %d %s, want 400 Ticket.InvalidTitle", rec.Code, rec.Body.String())
	}
}

// Regression: the customer's own reply set WAITING_REPLY ("待您回复").
func TestCustomerReplyDoesNotWaitOnCustomer(t *testing.T) {
	a := newMainPathApp(t)
	create := httptest.NewRequest(http.MethodPost, "/internal/tickets",
		strings.NewReader(`{"category":"billing","priority":"NORMAL","message":"hello"}`))
	create.Header.Set("X-Euler-Account-Id", "100123")
	crec := httptest.NewRecorder()
	a.handleCreate(crec, create)
	var created Ticket
	if err := json.Unmarshal(crec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	reply := func() TicketStatus {
		req := httptest.NewRequest(http.MethodPost, "/internal/tickets/"+created.TicketID+"/reply",
			strings.NewReader(`{"message":"more detail"}`))
		req.Header.Set("X-Euler-Account-Id", "100123")
		rec := httptest.NewRecorder()
		a.handleReply(rec, req)
		var got Ticket
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("reply: %d %s", rec.Code, rec.Body.String())
		}
		return got.Status
	}
	if st := reply(); st != StatusOpen {
		t.Fatalf("customer reply on an unclaimed ticket: status %s, want OPEN", st)
	}
	// Support asks something; the customer's answer hands it back to support.
	if _, err := a.store.Reply(100123, created.TicketID, Message{Author: "agent", FromUser: false, Body: "which invoice?"}, a.now()); err != nil {
		t.Fatal(err)
	}
	if st := reply(); st != StatusProcessing {
		t.Fatalf("customer answer to support: status %s, want PROCESSING", st)
	}
}

// B5: contact had no length check, so a value wider than ticket.contact
// (VARCHAR(128)) was a 500 in SQL mode. It is a 400 now; 128 characters fit.
func TestAPICreateRejectsOverlongContact(t *testing.T) {
	a := newMainPathApp(t)
	rec, _ := apiCreate(t, a, `{"category":"咨询","message":"x","contact":"`+strings.Repeat("邮", maxContactRunes+1)+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Ticket.InvalidContact") {
		t.Fatalf("overlong contact: %d %s, want 400 Ticket.InvalidContact", rec.Code, rec.Body.String())
	}
	full := strings.Repeat("邮", maxContactRunes)
	rec, dto := apiCreate(t, a, `{"category":"咨询","message":"x","contact":"  `+full+`  "}`)
	if rec.Code != http.StatusCreated || dto.Contact != full {
		t.Fatalf("128-character contact: %d %s", rec.Code, rec.Body.String())
	}
}

// The column really holds 128 characters: the limit is the schema's.
func TestSQLAPICreateStoresFullWidthContact(t *testing.T) {
	a := newApp(newSQLTestStore(t))
	full := strings.Repeat("邮", maxContactRunes)
	rec, dto := apiCreate(t, a, `{"category":"咨询","message":"x","contact":"`+full+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	got, err := a.store.Get(100123, dto.TicketID)
	if err != nil || got.Contact != full {
		t.Fatalf("stored contact = %q, %v", got.Contact, err)
	}
	rec, _ = apiCreate(t, a, `{"category":"咨询","message":"x","contact":"`+full+`x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("129 characters against MySQL: %d %s, want 400 (not a 500)", rec.Code, rec.Body.String())
	}
}

// internalCreate posts to the internal POST /internal/tickets handler.
func internalCreate(t *testing.T, a *app, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/tickets", strings.NewReader(body))
	req.Header.Set("X-Euler-Account-Id", "100123")
	rec := httptest.NewRecorder()
	a.handleCreate(rec, req)
	return rec
}

// L1: category had no length check, so a value wider than ticket.category
// (VARCHAR(32)) was a 500 in SQL mode. Both create endpoints answer 400 now;
// 32 characters (after trimming) fit.
func TestCreateRejectsOverlongCategory(t *testing.T) {
	a := newMainPathApp(t)
	long := strings.Repeat("类", maxCategoryRunes+1)
	full := strings.Repeat("类", maxCategoryRunes)

	rec, _ := apiCreate(t, a, `{"category":"`+long+`","message":"x"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Ticket.InvalidCategory") {
		t.Fatalf("api overlong category: %d %s, want 400 Ticket.InvalidCategory", rec.Code, rec.Body.String())
	}
	rec, dto := apiCreate(t, a, `{"category":"  `+full+`  ","message":"x"}`)
	if rec.Code != http.StatusCreated || dto.Category != full {
		t.Fatalf("api 32-character category: %d %s", rec.Code, rec.Body.String())
	}

	rec = internalCreate(t, a, `{"category":"`+long+`","message":"x"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Ticket.InvalidCategory") {
		t.Fatalf("internal overlong category: %d %s, want 400 Ticket.InvalidCategory", rec.Code, rec.Body.String())
	}
	rec = internalCreate(t, a, `{"category":"`+full+`","message":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("internal 32-character category: %d %s", rec.Code, rec.Body.String())
	}
}

// The column really holds 32 characters: the limit is the schema's.
func TestSQLCreateCategoryWidth(t *testing.T) {
	a := newApp(newSQLTestStore(t))
	full := strings.Repeat("类", maxCategoryRunes)
	rec, dto := apiCreate(t, a, `{"category":"`+full+`","message":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	got, err := a.store.Get(100123, dto.TicketID)
	if err != nil || got.Category != full {
		t.Fatalf("stored category = %q, %v", got.Category, err)
	}
	rec, _ = apiCreate(t, a, `{"category":"`+full+`x","message":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("33 characters against MySQL (api): %d %s, want 400 (not a 500)", rec.Code, rec.Body.String())
	}
	rec = internalCreate(t, a, `{"category":"`+full+`x","message":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("33 characters against MySQL (internal): %d %s, want 400 (not a 500)", rec.Code, rec.Body.String())
	}
}
