package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/notify"
)

// brokenStore fails every call the way a lost database connection does.
type brokenStore struct{ *memStore }

var errDBDown = errors.New("dial tcp 10.0.0.12:3306: connect: connection refused")

func (brokenStore) Save(notify.Notification) error { return errDBDown }
func (brokenStore) FindByBizKey(int64, notify.Class, string) ([]notify.Notification, error) {
	return nil, errDBDown
}

func newTestApp(store notify.Store) *app {
	disp := notify.NewDispatcher(store, time.Now, nil)
	disp.Register(&mockSender{ch: notify.ChannelInApp})
	return &app{disp: disp, store: store}
}

func post(a *app, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/internal/notifications", bytes.NewBufferString(body))
	req.Header.Set("X-Euler-Account-Id", "100123")
	rr := httptest.NewRecorder()
	a.handleNotifications(rr, req)
	return rr
}

func verify(a *app, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/internal/notifications/verify?"+query, nil)
	req.Header.Set("X-Euler-Account-Id", "100123")
	rr := httptest.NewRecorder()
	a.handleVerify(rr, req)
	return rr
}

// Store/database failures used to come back as 400 Common.InvalidParameter
// carrying the raw error text (hosts, SQL). They are server errors.
func TestStoreFailureIs500WithoutDetail(t *testing.T) {
	a := newTestApp(brokenStore{newMemStore()})

	rr := post(a, `{"Class":"ALERT","TemplateId":"t","Channels":["IN_APP"]}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("dispatch on a broken store: %d %s, want 500", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "10.0.0.12") || strings.Contains(rr.Body.String(), "InvalidParameter") {
		t.Fatalf("dispatch leaked the store error or blamed the caller: %s", rr.Body.String())
	}

	rr = verify(a, "class=RELEASE_WARNING&bizKey=euecs-1")
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "10.0.0.12") {
		t.Fatalf("verify on a broken store: %d %s, want a generic 500", rr.Code, rr.Body.String())
	}
}

// Client mistakes stay 400.
func TestClientErrorsStay400(t *testing.T) {
	a := newTestApp(newMemStore())
	for name, body := range map[string]string{
		"missing biz key": `{"Class":"RELEASE_WARNING","TemplateId":"t"}`,
		"unknown class":   `{"Class":"BOGUS","TemplateId":"t"}`,
		"no class":        `{"TemplateId":"t"}`,
	} {
		if rr := post(a, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
		}
	}
	if rr := verify(a, "class=ALERT&bizKey=x"); rr.Code != http.StatusBadRequest {
		t.Fatalf("verify with a non-critical class: %d, want 400", rr.Code)
	}
	if rr := post(a, `{"Class":"ALERT","TemplateId":"t","Channels":["IN_APP"]}`); rr.Code != http.StatusOK {
		t.Fatalf("valid dispatch: %d %s", rr.Code, rr.Body.String())
	}
}

// F3: production refuses to run the EMAIL channel without a relay; outside
// production the dev logger stands in; a configured relay is the real sender.
func TestEmailSenderSelection(t *testing.T) {
	for _, v := range []string{notify.SMTPAddrEnv, notify.SMTPUsernameEnv, notify.SMTPPasswordEnv, notify.SMTPFromEnv, notify.SMTPTLSEnv} {
		t.Setenv(v, "")
	}
	t.Setenv("EULER_ENV", "prod")
	if _, err := emailSender(); err == nil {
		t.Fatal("production without EULER_SMTP_ADDR must be a startup error")
	}

	t.Setenv("EULER_ENV", "dev")
	s, err := emailSender()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(devLogEmailSender); !ok {
		t.Fatalf("dev without SMTP: %T, want the logging stand-in", s)
	}

	t.Setenv("EULER_ENV", "prod")
	t.Setenv(notify.SMTPAddrEnv, "smtp.example.com:587")
	t.Setenv(notify.SMTPFromEnv, "Euler <noreply@euler.emoera.com>")
	t.Setenv(notify.SMTPUsernameEnv, "relay")
	t.Setenv(notify.SMTPPasswordEnv, "secret")
	s, err = emailSender()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*notify.SMTPSender); !ok {
		t.Fatalf("configured relay: %T, want *notify.SMTPSender", s)
	}

	t.Setenv(notify.SMTPTLSEnv, "none") // credentials in plaintext to a remote relay
	if _, err := emailSender(); err == nil {
		t.Fatal("an invalid SMTP configuration must be a startup error")
	}
}

// A verification code is a known, trust-critical class: dispatched over EMAIL
// and verifiable, with the code redacted from the record.
func TestVerificationCodeDispatch(t *testing.T) {
	store := newMemStore()
	disp := notify.NewDispatcher(store, time.Now, nil)
	rec := &recordingSender{}
	disp.Register(rec)
	a := &app{disp: disp, store: store}

	rr := post(a, `{"Class":"VERIFICATION_CODE","TemplateId":"auth.password-reset-code","BizKey":"pwreset:100123",`+
		`"Params":{"to":"alice@example.com","secret.code":"493817","ttlMinutes":"10"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"Code":"OK"`) {
		t.Fatalf("dispatch: %d %s", rr.Code, rr.Body.String())
	}
	if rec.code != "493817" {
		t.Fatalf("the email sender did not get the code: %q", rec.code)
	}
	if strings.Contains(rr.Body.String(), "493817") {
		t.Fatal("the response echoed the code")
	}
	ns, _ := store.ListByAccount(100123)
	if len(ns) != 1 || ns[0].Params["secret.code"] != notify.RedactedValue {
		t.Fatalf("persisted record: %+v", ns)
	}
	if rr := verify(a, "class=VERIFICATION_CODE&bizKey=pwreset:100123"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"Delivered":true`) {
		t.Fatalf("verify: %d %s", rr.Code, rr.Body.String())
	}
}

// The registration code (svc-iam web-auth, template auth.register-code) is
// rendered by the built-in template with the real code, which stays out of
// the record; without a code it is not reported as delivered.
func TestRegisterCodeDevEmail(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := newMemStore()
	disp := notify.NewDispatcher(store, time.Now, nil)
	disp.Register(devLogEmailSender{})
	a := &app{disp: disp, store: store}

	rr := post(a, `{"Class":"VERIFICATION_CODE","TemplateId":"`+notify.TemplateRegisterCode+`","BizKey":"register:new@example.com",`+
		`"Params":{"to":"new@example.com","secret.code":"602113","ttlMinutes":"10"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"Code":"OK"`) || strings.Contains(rr.Body.String(), "602113") {
		t.Fatalf("dispatch: %d %s", rr.Code, rr.Body.String())
	}
	if out := logs.String(); !strings.Contains(out, "【Euler】注册验证码") || !strings.Contains(out, "602113") || !strings.Contains(out, "10 分钟内有效") {
		t.Fatalf("dev mail: %s", out)
	}

	rr = post(a, `{"Class":"VERIFICATION_CODE","TemplateId":"`+notify.TemplateRegisterCode+`","BizKey":"register:other@example.com",`+
		`"Params":{"to":"other@example.com"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"Code":"Notify.NotDelivered"`) {
		t.Fatalf("dispatch without a code: %d %s", rr.Code, rr.Body.String())
	}
	ns, _ := store.ListByAccount(100123)
	for _, n := range ns {
		if n.Params["secret.code"] != "" && n.Params["secret.code"] != notify.RedactedValue {
			t.Fatalf("persisted record carries the code: %+v", n)
		}
	}
}

type recordingSender struct{ code string }

func (*recordingSender) Channel() notify.Channel { return notify.ChannelEmail }

func (s *recordingSender) Send(n notify.Notification) (notify.Delivery, error) {
	s.code = n.Params["secret.code"]
	return notify.Delivery{Channel: notify.ChannelEmail, Success: true, Provider: "test"}, nil
}
