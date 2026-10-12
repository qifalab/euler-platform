package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"mime"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a minimal SMTP server: just enough protocol for net/smtp -----------------

type fakeSMTPOpts struct {
	implicitTLS   bool // TLS from the first byte
	offerStartTLS bool
	offerAuth     bool
}

type fakeSMTP struct {
	opts fakeSMTPOpts
	ln   net.Listener
	cert tls.Certificate
	pool *x509.CertPool

	mu       sync.Mutex
	auth     string // decoded AUTH PLAIN response
	from     string
	rcpt     []string
	data     string
	tlsAtTxn bool // was the connection TLS when MAIL FROM arrived
	done     chan struct{}
}

func startFakeSMTP(t *testing.T, opts fakeSMTPOpts) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, pool := selfSignedCert(t)
	f := &fakeSMTP{opts: opts, ln: ln, cert: cert, pool: pool, done: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serveOne()
	return f
}

func (f *fakeSMTP) serveOne() {
	defer close(f.done)
	c, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	srvTLS := &tls.Config{Certificates: []tls.Certificate{f.cert}}
	isTLS := false
	if f.opts.implicitTLS {
		tc := tls.Server(c, srvTLS)
		if err := tc.Handshake(); err != nil {
			return
		}
		c, isTLS = tc, true
	}
	r := bufio.NewReader(c)
	write := func(lines ...string) {
		for _, l := range lines {
			_, _ = io.WriteString(c, l+"\r\n")
		}
	}
	write("220 fake.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			lines := []string{"250-fake.test"}
			if f.opts.offerStartTLS && !isTLS {
				lines = append(lines, "250-STARTTLS")
			}
			if f.opts.offerAuth {
				lines = append(lines, "250-AUTH PLAIN")
			}
			write(append(lines, "250 8BITMIME")...)
		case upper == "STARTTLS":
			write("220 2.0.0 ready")
			tc := tls.Server(c, srvTLS)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, isTLS = tc, true
			r = bufio.NewReader(c)
		case strings.HasPrefix(upper, "AUTH PLAIN "):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN "):]))
			f.mu.Lock()
			f.auth = string(raw)
			f.mu.Unlock()
			write("235 2.7.0 authenticated")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			f.mu.Lock()
			// Keep the path, drop ESMTP parameters (BODY=8BITMIME).
			f.from, f.tlsAtTxn = strings.Fields(line[len("MAIL FROM:"):])[0], isTLS
			f.mu.Unlock()
			write("250 2.1.0 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, line[len("RCPT TO:"):])
			f.mu.Unlock()
			write("250 2.1.5 ok")
		case upper == "DATA":
			write("354 end with <CRLF>.<CRLF>")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			write("250 2.0.0 queued as 42")
		case upper == "QUIT":
			write("221 2.0.0 bye")
			return
		case upper == "RSET", upper == "NOOP":
			write("250 ok")
		default:
			write("502 5.5.2 unknown command")
		}
	}
}

func (f *fakeSMTP) wait(t *testing.T) {
	t.Helper()
	_ = f.ln.Close()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake SMTP server did not finish")
	}
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func resetCodeNotification() Notification {
	return Notification{
		AccountID: acct, Class: ClassVerificationCode, TemplateID: TemplatePasswordResetCode,
		Params:   map[string]string{ParamRecipient: "alice@example.com", ParamCode: "493817", ParamTTLMinutes: "10"},
		Channels: ChannelsFor(ClassVerificationCode), BizKey: "pwreset:100123",
	}
}

// decodeMail parses what the relay received and returns the decoded subject
// and plain-text body.
func decodeMail(t *testing.T, data string) (*mail.Message, string, string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("relay received an unparsable message: %v\n%s", err, data)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(raw), "\r\n", ""))
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	return msg, subject, string(body)
}

// F3: a verification code goes out through a real SMTP conversation —
// STARTTLS before AUTH, the code in the body — while the persisted record
// keeps the code redacted.
func TestSMTPSenderStartTLSAuthAndRedactedRecord(t *testing.T) {
	srv := startFakeSMTP(t, fakeSMTPOpts{offerStartTLS: true, offerAuth: true})
	sender, err := NewSMTPSender(SMTPConfig{
		Addr: srv.ln.Addr().String(), Username: "relay-user", Password: "relay-pass",
		From: "Euler 云平台 <noreply@euler.emoera.com>", TLS: SMTPStartTLS,
		TLSConfig: &tls.Config{RootCAs: srv.pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := newStore()
	d := NewDispatcher(store, func() time.Time { return base }, nil)
	d.Register(sender)

	sent, err := d.Send(resetCodeNotification())
	if err != nil {
		t.Fatalf("send: %v (%+v)", err, sent)
	}
	srv.wait(t)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.tlsAtTxn {
		t.Fatal("the transaction ran before STARTTLS")
	}
	if srv.auth != "\x00relay-user\x00relay-pass" {
		t.Fatalf("AUTH PLAIN = %q", srv.auth)
	}
	if srv.from != "<noreply@euler.emoera.com>" || len(srv.rcpt) != 1 || srv.rcpt[0] != "<alice@example.com>" {
		t.Fatalf("envelope from=%q rcpt=%v", srv.from, srv.rcpt)
	}
	msg, subject, body := decodeMail(t, srv.data)
	if !strings.Contains(subject, "验证码") || !strings.Contains(body, "493817") || !strings.Contains(body, "10 分钟") {
		t.Fatalf("rendered mail: subject %q body %q", subject, body)
	}
	if got := msg.Header.Get("Message-ID"); got == "" || got != sent.Deliveries[0].MessageID {
		t.Fatalf("Message-ID %q, delivery evidence %q", got, sent.Deliveries[0].MessageID)
	}
	if !sent.Deliveries[0].Success || sent.Deliveries[0].Provider != "smtp:127.0.0.1" {
		t.Fatalf("delivery evidence: %+v", sent.Deliveries[0])
	}

	// The record proves a code was mailed, never which one.
	rec, err := store.Get(sent.NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusSent || rec.Params[ParamCode] != RedactedValue || rec.Params[ParamRecipient] != "alice@example.com" {
		t.Fatalf("persisted record: %+v", rec)
	}
	if sent.Params[ParamCode] != RedactedValue {
		t.Fatal("the dispatcher handed the secret back to the caller")
	}
}

func TestSMTPSenderImplicitTLS(t *testing.T) {
	srv := startFakeSMTP(t, fakeSMTPOpts{implicitTLS: true})
	sender, err := NewSMTPSender(SMTPConfig{
		Addr: srv.ln.Addr().String(), From: "noreply@euler.emoera.com", TLS: SMTPImplicitTLS,
		TLSConfig: &tls.Config{RootCAs: srv.pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := resetCodeNotification()
	n.NotificationID = "ntf-1"
	if del, err := sender.Send(n); err != nil || !del.Success {
		t.Fatalf("implicit TLS send: %+v %v", del, err)
	}
	srv.wait(t)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.tlsAtTxn || srv.data == "" {
		t.Fatalf("tls=%v data=%q", srv.tlsAtTxn, srv.data)
	}
}

// A relay that does not offer STARTTLS must not receive the message in
// plaintext: no silent downgrade.
func TestSMTPSenderRefusesDowngrade(t *testing.T) {
	srv := startFakeSMTP(t, fakeSMTPOpts{offerAuth: true})
	sender, err := NewSMTPSender(SMTPConfig{
		Addr: srv.ln.Addr().String(), Username: "u", Password: "p",
		From: "noreply@euler.emoera.com", TLS: SMTPStartTLS,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(resetCodeNotification()); !errors.Is(err, ErrSMTPNoStartTLS) {
		t.Fatalf("send without STARTTLS: %v, want ErrSMTPNoStartTLS", err)
	}
	srv.wait(t)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.auth != "" || srv.from != "" || srv.data != "" {
		t.Fatalf("credentials or mail leaked in plaintext: auth=%q from=%q", srv.auth, srv.from)
	}
}

// "none" is for a loopback relay (a local MTA / sidecar); net/smtp still
// sends AUTH PLAIN there.
func TestSMTPSenderPlaintextLoopback(t *testing.T) {
	srv := startFakeSMTP(t, fakeSMTPOpts{offerAuth: true})
	sender, err := NewSMTPSender(SMTPConfig{
		Addr: srv.ln.Addr().String(), Username: "u", Password: "p",
		From: "noreply@euler.emoera.com", TLS: SMTPNoTLS,
	})
	if err != nil {
		t.Fatal(err)
	}
	if del, err := sender.Send(resetCodeNotification()); err != nil || !del.Success {
		t.Fatalf("loopback send: %+v %v", del, err)
	}
	srv.wait(t)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.auth != "\x00u\x00p" || srv.tlsAtTxn {
		t.Fatalf("auth=%q tls=%v", srv.auth, srv.tlsAtTxn)
	}
}

func TestSMTPSenderRejectsBadRecipient(t *testing.T) {
	sender, err := NewSMTPSender(SMTPConfig{Addr: "127.0.0.1:1", From: "noreply@euler.emoera.com", TLS: SMTPNoTLS})
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"", "not-an-address", "a@example.com\r\nBcc: victim@example.com", "a@example.com, b@example.com"} {
		n := resetCodeNotification()
		n.Params[ParamRecipient] = to
		if _, err := sender.Send(n); !errors.Is(err, ErrNoRecipient) {
			t.Errorf("recipient %q: %v, want ErrNoRecipient", to, err)
		}
	}
}

func TestSMTPConfigFromEnv(t *testing.T) {
	set := func(addr, user, pass, from, mode string) {
		t.Setenv(SMTPAddrEnv, addr)
		t.Setenv(SMTPUsernameEnv, user)
		t.Setenv(SMTPPasswordEnv, pass)
		t.Setenv(SMTPFromEnv, from)
		t.Setenv(SMTPTLSEnv, mode)
	}
	set("", "", "", "", "")
	if _, ok, err := SMTPConfigFromEnv(); ok || err != nil {
		t.Fatalf("unset: ok=%v err=%v", ok, err)
	}
	set("smtp.example.com:587", "u", "p", "Euler <noreply@euler.emoera.com>", "")
	cfg, ok, err := SMTPConfigFromEnv()
	if !ok || err != nil || cfg.TLS != SMTPStartTLS {
		t.Fatalf("valid config: %+v %v %v (TLS must default to starttls)", cfg, ok, err)
	}
	for name, c := range map[string][5]string{
		"no port":                        {"smtp.example.com", "", "", "noreply@euler.emoera.com", ""},
		"unknown tls mode":               {"smtp.example.com:587", "", "", "noreply@euler.emoera.com", "ssl3"},
		"credentials in plaintext":       {"smtp.example.com:25", "u", "p", "noreply@euler.emoera.com", "none"},
		"password without user":          {"smtp.example.com:587", "", "p", "noreply@euler.emoera.com", ""},
		"missing from":                   {"smtp.example.com:587", "", "", "", ""},
		"from with an injected header":   {"smtp.example.com:587", "", "", "noreply@euler.emoera.com\r\nBcc: x@y.z", ""},
		"implicit tls, missing from too": {"smtp.example.com:465", "u", "p", "", "tls"},
	} {
		set(c[0], c[1], c[2], c[3], c[4])
		if _, ok, err := SMTPConfigFromEnv(); !ok || err == nil {
			t.Errorf("%s: accepted (ok=%v)", name, ok)
		}
	}
	// Credentials over "none" are fine to a loopback relay.
	set("127.0.0.1:25", "u", "p", "noreply@euler.emoera.com", "none")
	if _, ok, err := SMTPConfigFromEnv(); !ok || err != nil {
		t.Fatalf("loopback plaintext relay: %v", err)
	}
}

func TestVerificationCodeClass(t *testing.T) {
	c := ClassVerificationCode
	if !c.TrustCritical() || OptOutAllowed(c) {
		t.Fatal("a verification code must be trust-critical and not opt-out-able")
	}
	if ch := ChannelsFor(c); len(ch) != 1 || ch[0] != ChannelEmail {
		t.Fatalf("channels %v, want EMAIL only", ch)
	}
	// Never throttled: a storm of alerts does not suppress the code.
	d, _, senders := newDispatcher(base)
	for i := 0; i < PerTenantRateLimit+3; i++ {
		_, _ = d.Send(Notification{AccountID: acct, Class: ClassAlert, TemplateID: "a", Channels: []Channel{ChannelInApp}})
	}
	n, err := d.Send(resetCodeNotification())
	if err != nil || n.Status != StatusSent || senders[ChannelEmail].sent != 1 {
		t.Fatalf("code after an alert storm: %+v %v", n, err)
	}
}

func TestRedactSecrets(t *testing.T) {
	in := map[string]string{"to": "a@b.c", ParamCode: "123456", "secret.token": "t", "plain": "p"}
	out := RedactSecrets(in)
	if out["to"] != "a@b.c" || out["plain"] != "p" || out[ParamCode] != RedactedValue || out["secret.token"] != RedactedValue {
		t.Fatalf("redacted: %v", out)
	}
	if in[ParamCode] != "123456" {
		t.Fatal("RedactSecrets modified its input")
	}
	if RedactSecrets(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

// The verification-code mails have built-in Chinese templates: the code and
// its validity are rendered, never a placeholder.
func TestDefaultRendererCodeTemplates(t *testing.T) {
	for _, c := range []struct {
		template, subject, purpose string
	}{
		{TemplatePasswordResetCode, "【Euler】密码重置验证码", "重置 Euler 账号的登录密码"},
		{TemplateRegisterCode, "【Euler】注册验证码", "注册 Euler 云平台账号"},
	} {
		n := resetCodeNotification()
		n.TemplateID = c.template
		n.Params[ParamTTLMinutes] = "15"
		subject, body, err := DefaultRenderer(n)
		if err != nil {
			t.Fatalf("%s: %v", c.template, err)
		}
		if subject != c.subject || !strings.Contains(body, c.purpose) || !strings.Contains(body, "    493817\n") ||
			!strings.Contains(body, "验证码 15 分钟内有效") || strings.Contains(body, "<no value>") ||
			strings.Contains(body, "alice@example.com") {
			t.Fatalf("%s: subject %q body %q", c.template, subject, body)
		}
		// Without ttlMinutes the validity falls back to the platform's 10.
		delete(n.Params, ParamTTLMinutes)
		if _, body, err := DefaultRenderer(n); err != nil || !strings.Contains(body, "验证码 10 分钟内有效") {
			t.Fatalf("%s default TTL: %v %q", c.template, err, body)
		}
		// A missing code, or the redacted placeholder from a persisted
		// record, is refused rather than mailed.
		for _, code := range []string{"", "  ", RedactedValue} {
			n.Params[ParamCode] = code
			if _, _, err := DefaultRenderer(n); !errors.Is(err, ErrTemplateParam) {
				t.Errorf("%s with code %q: %v, want ErrTemplateParam", c.template, code, err)
			}
		}
		delete(n.Params, ParamCode)
		if _, _, err := DefaultRenderer(n); !errors.Is(err, ErrTemplateParam) {
			t.Errorf("%s without a code: %v, want ErrTemplateParam", c.template, err)
		}
	}
}

// The registration code goes out with the code in the mail while the record
// keeps it redacted; a record re-sent as is fails instead of mailing the
// placeholder.
func TestRegisterCodeMailedAndRedacted(t *testing.T) {
	srv := startFakeSMTP(t, fakeSMTPOpts{})
	sender, err := NewSMTPSender(SMTPConfig{Addr: srv.ln.Addr().String(), From: "noreply@euler.emoera.com", TLS: SMTPNoTLS})
	if err != nil {
		t.Fatal(err)
	}
	store := newStore()
	d := NewDispatcher(store, func() time.Time { return base }, nil)
	d.Register(sender)
	n := resetCodeNotification()
	n.TemplateID, n.BizKey = TemplateRegisterCode, "register:alice@example.com"
	sent, err := d.Send(n)
	if err != nil {
		t.Fatalf("send: %v (%+v)", err, sent)
	}
	srv.wait(t)
	srv.mu.Lock()
	_, subject, body := decodeMail(t, srv.data)
	srv.mu.Unlock()
	if subject != "【Euler】注册验证码" || !strings.Contains(body, "493817") || !strings.Contains(body, "10 分钟") {
		t.Fatalf("rendered mail: subject %q body %q", subject, body)
	}
	rec, err := store.Get(sent.NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Params[ParamCode] != RedactedValue {
		t.Fatalf("persisted record: %+v", rec)
	}
	if del, err := sender.Send(rec); !errors.Is(err, ErrTemplateParam) || del.Success {
		t.Fatalf("re-sending the redacted record: %+v %v, want ErrTemplateParam", del, err)
	}
}

func TestDefaultRendererGenericTemplate(t *testing.T) {
	subject, body, err := DefaultRenderer(Notification{Class: ClassExpiry, TemplateID: "expiry-7d", BizKey: "euecs-1",
		Params: map[string]string{ParamRecipient: "a@b.c", "resourceId": "euecs-1", "days": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subject, "expiry-7d") || !strings.Contains(body, "days: 7") || !strings.Contains(body, "resourceId: euecs-1") || strings.Contains(body, "a@b.c") {
		t.Fatalf("generic rendering: %q / %q", subject, body)
	}
}
