package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"
)

// Real email delivery over SMTP (the mail relay behind ChannelEmail).
//
// Configuration is environment-driven so the same binary runs everywhere:
//
//	EULER_SMTP_ADDR      relay host:port (unset = no SMTP; see SMTPConfigFromEnv)
//	EULER_SMTP_USERNAME  AUTH PLAIN user (optional; empty = no AUTH)
//	EULER_SMTP_PASSWORD  AUTH PLAIN password
//	EULER_SMTP_FROM      envelope + header sender, "Name <addr>" or "addr"
//	EULER_SMTP_TLS       starttls (default) | tls (implicit, usually :465) | none
//
// Transport security fails closed: in starttls mode a relay that does not
// offer STARTTLS is an error, never a silent downgrade to plaintext, and
// credentials are refused over "none" unless the relay is on loopback.
const (
	SMTPAddrEnv     = "EULER_SMTP_ADDR"
	SMTPUsernameEnv = "EULER_SMTP_USERNAME"
	SMTPPasswordEnv = "EULER_SMTP_PASSWORD"
	SMTPFromEnv     = "EULER_SMTP_FROM"
	SMTPTLSEnv      = "EULER_SMTP_TLS"
)

// SMTPTLSMode selects how the connection to the relay is secured.
type SMTPTLSMode string

const (
	SMTPStartTLS    SMTPTLSMode = "starttls" // plaintext connect, mandatory STARTTLS upgrade
	SMTPImplicitTLS SMTPTLSMode = "tls"      // TLS from the first byte (SMTPS)
	SMTPNoTLS       SMTPTLSMode = "none"     // plaintext; credentials only on loopback
)

// DefaultSMTPTimeout bounds one whole delivery (connect → QUIT).
const DefaultSMTPTimeout = 15 * time.Second

// Template ids and parameters with a built-in rendering (DefaultRenderer).
const (
	// TemplatePasswordResetCode is the password-reset verification code mail.
	TemplatePasswordResetCode = "auth.password-reset-code"
	// TemplateRegisterCode is the sign-up email verification code mail
	// (svc-iam web-auth register.go).
	TemplateRegisterCode = "auth.register-code"
	// ParamCode is the one-time code; secret, so it is redacted in the record.
	ParamCode = SecretParamPrefix + "code"
	// ParamTTLMinutes is how long the code stays valid.
	ParamTTLMinutes = "ttlMinutes"
)

// Errors returned by the SMTP sender.
var (
	ErrNoRecipient     = errors.New("notify: no valid recipient address (param \"" + ParamRecipient + "\")")
	ErrSMTPNoStartTLS  = errors.New("notify: SMTP relay does not offer STARTTLS (refusing to send in plaintext)")
	ErrSMTPNoAuth      = errors.New("notify: SMTP relay does not offer AUTH but credentials are configured")
	ErrSMTPInvalidConf = errors.New("notify: invalid SMTP configuration")
	// ErrTemplateParam: a built-in template lacks a parameter it cannot be
	// sent without (e.g. the code of a verification mail), or got the
	// redacted placeholder of a secret one — a mail that says "your code is
	// [redacted]" must fail, not be delivered.
	ErrTemplateParam = errors.New("notify: template parameter missing")
)

// SMTPConfig configures an SMTPSender.
type SMTPConfig struct {
	Addr     string // host:port of the relay
	Username string // empty disables AUTH
	Password string
	From     string // "Name <addr>" or "addr"
	TLS      SMTPTLSMode
	// Timeout bounds one delivery; zero means DefaultSMTPTimeout.
	Timeout time.Duration
	// TLSConfig overrides the client TLS settings (tests pin a private CA).
	// ServerName defaults to the host of Addr; certificates are always
	// verified.
	TLSConfig *tls.Config
	// HelloName is the EHLO name; empty means "localhost".
	HelloName string
}

// SMTPConfigFromEnv reads the EULER_SMTP_* variables. configured is false
// when EULER_SMTP_ADDR is unset — the caller decides whether that is
// acceptable (svc-notify refuses it in production). A set but invalid
// configuration is an error.
func SMTPConfigFromEnv() (cfg SMTPConfig, configured bool, err error) {
	cfg = SMTPConfig{
		Addr:     strings.TrimSpace(os.Getenv(SMTPAddrEnv)),
		Username: strings.TrimSpace(os.Getenv(SMTPUsernameEnv)),
		Password: os.Getenv(SMTPPasswordEnv),
		From:     strings.TrimSpace(os.Getenv(SMTPFromEnv)),
		TLS:      SMTPTLSMode(strings.ToLower(strings.TrimSpace(os.Getenv(SMTPTLSEnv)))),
	}
	if cfg.Addr == "" {
		return SMTPConfig{}, false, nil
	}
	if cfg.TLS == "" {
		cfg.TLS = SMTPStartTLS
	}
	if _, err := cfg.validate(); err != nil {
		return SMTPConfig{}, true, err
	}
	return cfg, true, nil
}

// validate checks the configuration and returns the parsed sender address.
func (c SMTPConfig) validate() (*mail.Address, error) {
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("%w: %s must be host:port", ErrSMTPInvalidConf, SMTPAddrEnv)
	}
	switch c.TLS {
	case SMTPStartTLS, SMTPImplicitTLS, SMTPNoTLS:
	default:
		return nil, fmt.Errorf("%w: %s must be starttls, tls or none", ErrSMTPInvalidConf, SMTPTLSEnv)
	}
	if c.TLS == SMTPNoTLS && c.Username != "" && !isLoopbackHost(host) {
		return nil, fmt.Errorf("%w: credentials over a plaintext connection (%s=none) are only allowed to a loopback relay", ErrSMTPInvalidConf, SMTPTLSEnv)
	}
	if c.Username == "" && c.Password != "" {
		return nil, fmt.Errorf("%w: %s is set without %s", ErrSMTPInvalidConf, SMTPPasswordEnv, SMTPUsernameEnv)
	}
	from, err := parseAddress(c.From)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be an email address", ErrSMTPInvalidConf, SMTPFromEnv)
	}
	return from, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseAddress accepts one RFC 5322 address and nothing that could smuggle a
// header (CR/LF) or a second recipient.
func parseAddress(s string) (*mail.Address, error) {
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("invalid address")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address == "" || strings.ContainsAny(a.Address, "\r\n<>,;") {
		return nil, errors.New("invalid address")
	}
	return a, nil
}

// Renderer turns a notification into an email subject and plain-text body.
type Renderer func(n Notification) (subject, body string, err error)

// builtinTemplate is a dedicated rendering: subject, text/template body and
// the parameters it cannot be rendered without.
type builtinTemplate struct {
	subject, body string
	required      []string
}

// codeLine and codeTTLLine render a verification code (ParamCode, a secret
// parameter: the dispatcher hands the senders the real value and persists it
// redacted) and its validity (ParamTTLMinutes, default 10). Keys of Params are
// reached with {{index . "key"}} (secret keys contain a dot).
const (
	codeLine     = "    {{index . \"" + ParamCode + "\"}}\n\n"
	codeTTLLine  = "验证码 {{with index . \"" + ParamTTLMinutes + "\"}}{{.}}{{else}}10{{end}} 分钟内有效，请勿告知他人。\n"
	systemFooter = "— Euler 云平台（系统邮件，请勿回复）\n"
)

// builtinTemplates are the template ids with a dedicated rendering.
var builtinTemplates = map[string]builtinTemplate{
	TemplatePasswordResetCode: {
		subject: "【Euler】密码重置验证码",
		body: "您好：\n\n" +
			"您正在重置 Euler 账号的登录密码，验证码为：\n\n" +
			codeLine + codeTTLLine +
			"如果这不是您本人的操作，请忽略本邮件，并尽快检查账号安全。\n\n" +
			systemFooter,
		required: []string{ParamCode},
	},
	TemplateRegisterCode: {
		subject: "【Euler】注册验证码",
		body: "您好：\n\n" +
			"您正在使用本邮箱注册 Euler 云平台账号，验证码为：\n\n" +
			codeLine + codeTTLLine +
			"如果这不是您本人的操作，请忽略本邮件，切勿将验证码提供给任何人。\n\n" +
			systemFooter,
		required: []string{ParamCode},
	},
}

// DefaultRenderer renders the built-in templates, and any other template id
// as a generic message listing its parameters (sorted, recipient omitted),
// so a class without a dedicated template still delivers its content.
func DefaultRenderer(n Notification) (string, string, error) {
	if t, ok := builtinTemplates[n.TemplateID]; ok {
		for _, k := range t.required {
			if v := strings.TrimSpace(n.Params[k]); v == "" || v == RedactedValue {
				return "", "", fmt.Errorf("%w: %s needs %q", ErrTemplateParam, n.TemplateID, k)
			}
		}
		tmpl, err := template.New(n.TemplateID).Option("missingkey=zero").Parse(t.body)
		if err != nil {
			return "", "", err
		}
		var buf bytes.Buffer
		params := n.Params
		if params == nil {
			params = map[string]string{}
		}
		if err := tmpl.Execute(&buf, params); err != nil {
			return "", "", err
		}
		return t.subject, buf.String(), nil
	}
	keys := make([]string, 0, len(n.Params))
	for k := range n.Params {
		if k != ParamRecipient {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "Euler 云平台通知（%s）\n\n", n.Class)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", strings.TrimPrefix(k, SecretParamPrefix), n.Params[k])
	}
	if n.BizKey != "" {
		fmt.Fprintf(&b, "\n关联对象: %s\n", n.BizKey)
	}
	return "【Euler】" + n.TemplateID, b.String(), nil
}

// SMTPSender delivers ChannelEmail messages through an SMTP relay.
type SMTPSender struct {
	cfg    SMTPConfig
	host   string
	from   *mail.Address
	render Renderer
	dialer net.Dialer
	now    func() time.Time
}

var _ Sender = (*SMTPSender)(nil)

// NewSMTPSender validates cfg and returns a sender using DefaultRenderer.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if cfg.TLS == "" {
		cfg.TLS = SMTPStartTLS
	}
	from, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultSMTPTimeout
	}
	if cfg.HelloName == "" {
		cfg.HelloName = "localhost"
	}
	host, _, _ := net.SplitHostPort(cfg.Addr)
	return &SMTPSender{cfg: cfg, host: host, from: from, render: DefaultRenderer, now: time.Now}, nil
}

// WithRenderer replaces the renderer (nil keeps the current one).
func (s *SMTPSender) WithRenderer(r Renderer) *SMTPSender {
	if r != nil {
		s.render = r
	}
	return s
}

// Channel implements Sender.
func (s *SMTPSender) Channel() Channel { return ChannelEmail }

// Send implements Sender: one SMTP transaction per notification. The
// Delivery names the relay and the Message-ID it accepted — the dispute
// evidence the notification record keeps.
func (s *SMTPSender) Send(n Notification) (Delivery, error) {
	now := s.now()
	del := Delivery{Channel: ChannelEmail, SentAt: now, Provider: "smtp:" + s.host}
	to, err := parseAddress(strings.TrimSpace(n.Params[ParamRecipient]))
	if err != nil {
		return del, ErrNoRecipient
	}
	subject, body, err := s.render(n)
	if err != nil {
		return del, fmt.Errorf("notify: render %s: %w", n.TemplateID, err)
	}
	msgID, err := s.messageID()
	if err != nil {
		return del, err
	}
	msg := buildMessage(s.from, to, subject, body, msgID, now)
	if err := s.deliver(to.Address, msg); err != nil {
		return del, err
	}
	del.Success = true
	del.MessageID = msgID
	return del, nil
}

func (s *SMTPSender) messageID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("notify: message id: %w", err)
	}
	domain := "euler.local"
	if at := strings.LastIndexByte(s.from.Address, '@'); at >= 0 && at < len(s.from.Address)-1 {
		domain = s.from.Address[at+1:]
	}
	return "<" + hex.EncodeToString(b) + "@" + domain + ">", nil
}

func (s *SMTPSender) tlsConfig() *tls.Config {
	var c *tls.Config
	if s.cfg.TLSConfig != nil {
		c = s.cfg.TLSConfig.Clone()
	} else {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if c.ServerName == "" {
		c.ServerName = s.host
	}
	return c
}

// deliver runs the SMTP conversation under one deadline.
func (s *SMTPSender) deliver(rcpt string, msg []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	conn, err := s.dialer.DialContext(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("notify: smtp dial: %w", err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if s.cfg.TLS == SMTPImplicitTLS {
		tc := tls.Client(conn, s.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("notify: smtp tls: %w", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("notify: smtp greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello(s.cfg.HelloName); err != nil {
		return fmt.Errorf("notify: smtp hello: %w", err)
	}
	if s.cfg.TLS == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return ErrSMTPNoStartTLS
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			return fmt.Errorf("notify: smtp starttls: %w", err)
		}
	}
	if s.cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return ErrSMTPNoAuth
		}
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.host)); err != nil {
			return fmt.Errorf("notify: smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return fmt.Errorf("notify: smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(rcpt); err != nil {
		return fmt.Errorf("notify: smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify: smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("notify: smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: smtp end of data: %w", err)
	}
	// The relay accepted the message at end-of-data; a failed QUIT does not
	// un-send it.
	_ = c.Quit()
	return nil
}

// buildMessage renders an RFC 5322 message: UTF-8 plain text, base64 body
// (any byte the template produced is transport-safe), encoded-word subject.
func buildMessage(from, to *mail.Address, subject, body, msgID string, now time.Time) []byte {
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", to.String())
	header("Subject", mime.BEncoding.Encode("UTF-8", subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", msgID)
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	header("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.Bytes()
}
