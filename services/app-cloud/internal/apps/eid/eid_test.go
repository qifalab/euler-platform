package eid

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
	_ "modernc.org/sqlite"
)

type testFixture struct {
	m       *Module
	trust   *trust.Module
	rt      *appkit.Runtime
	path    string
	enabled bool
}

func fixture(t *testing.T) *testFixture {
	t.Helper()
	for _, k := range []string{"EULER_EID_WECOM_URL", "EULER_EID_FEISHU_URL", "EULER_NOTIFY_WECOM_URL", "EULER_NOTIFY_FEISHU_URL", "EULER_EID_SMTP_ADDRESS", "EULER_EID_SMTP_FROM", "EULER_EID_SMTP_USERNAME", "EULER_EID_SMTP_PASSWORD"} {
		t.Setenv(k, "")
	}
	f := &testFixture{path: filepath.Join(t.TempDir(), "eid.db"), enabled: true}
	block, _ := aes.NewCipher(bytes.Repeat([]byte{7}, 32))
	aead, _ := cipher.NewGCM(block)
	f.rt = &appkit.Runtime{PublicURL: "https://console.example.test", Encrypt: func(text, aad string) ([]byte, error) {
		nonce := make([]byte, aead.NonceSize())
		if _, e := rand.Read(nonce); e != nil {
			return nil, e
		}
		return aead.Seal(nonce, nonce, []byte(text), []byte(aad)), nil
	}, Decrypt: func(b []byte, aad string) (string, error) {
		if len(b) < aead.NonceSize() {
			return "", errors.New("invalid cipher")
		}
		text, e := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(aad))
		return string(text), e
	}, DeriveKey: func(purpose string) []byte { k := sha256.Sum256([]byte("test:" + purpose)); return k[:] }, ApplicationEnabled: func(context.Context, string, string, string) (bool, error) { return f.enabled, nil }}
	f.open(t)
	t.Cleanup(func() { f.rt.DB.Close() })
	return f
}
func (f *testFixture) open(t *testing.T) {
	t.Helper()
	db, e := sql.Open("sqlite", f.path)
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	f.rt.DB = db
	if _, e = db.Exec(`CREATE TABLE IF NOT EXISTS audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER)`); e != nil {
		t.Fatal(e)
	}
	f.m = New(f.rt)
	if e = f.m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.trust = trust.New(f.rt)
	if e = f.trust.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func actor(id string, permissions ...string) appkit.Scope {
	return appkit.Scope{ActorID: id, ActorName: "User " + id, Email: id + "@example.test", EmailVerified: true, TenantID: "team-a", ProjectID: "project-a", InstallationID: "eid-a", ApplicationID: "eid", Permissions: permissions}
}
func call(t *testing.T, module appkit.Module, s appkit.Scope, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if value != nil {
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(appkit.WithScope(r.Context(), s))
	w := httptest.NewRecorder()
	module.Handler().ServeHTTP(w, r)
	return w
}
func status(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("want HTTP %d, got %d: %s", want, w.Code, w.Body.String())
	}
}
func result[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func apply(t *testing.T, f *testFixture, s appkit.Scope) Verification {
	t.Helper()
	w := call(t, f.m, s, "POST", "/verifications", verificationInput{RealName: "Private Alice", StudentID: "private-student-42", IdentityTitle: "研发团队成员", IdentityType: "core"})
	status(t, w, 201)
	return result[Verification](t, w)
}
func saveCfg(t *testing.T, f *testFixture, cfg Settings) {
	t.Helper()
	s := actor("owner", "read", "write", "manage", "secrets")
	status(t, call(t, f.m, s, "PUT", "/settings", cfg), 200)
}
func TestIdentityWorkflowPersistenceAndAuthorization(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	bob := actor("bob", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	owner := actor("owner", "read", "write", "manage", "secrets", "admin")
	v := apply(t, f, alice)
	status(t, call(t, f.m, alice, "POST", "/verifications", verificationInput{RealName: "Alice", StudentID: "42", IdentityTitle: "another", IdentityType: "active"}), 409)
	status(t, call(t, f.m, owner, "GET", "/review/verifications", nil), 403)
	viewer := actor("viewer", "read")
	status(t, call(t, f.m, viewer, "POST", "/verifications", verificationInput{}), 403)
	status(t, call(t, f.m, alice, "GET", "/review/verifications", nil), 403)
	foreign := reviewer
	foreign.ProjectID = "project-b"
	foreign.InstallationID = "eid-b"
	status(t, call(t, f.m, foreign, "POST", "/review/verifications/"+v.ID+"/decision", map[string]any{"status": "approved", "version": v.Version}), 404)
	w := call(t, f.m, reviewer, "POST", "/review/verifications/"+v.ID+"/decision", map[string]any{"status": "approved", "version": v.Version})
	status(t, w, 200)
	v = result[Verification](t, w)
	cards := result[struct {
		Items []Verification `json:"items"`
	}](t, call(t, f.m, alice, "GET", "/cards", nil))
	if len(cards.Items) != 1 {
		t.Fatal("approved identity missing")
	}
	if len(result[struct {
		Items []Verification `json:"items"`
	}](t, call(t, f.m, bob, "GET", "/verifications", nil)).Items) != 0 {
		t.Fatal("personal application leaked")
	}
	status(t, call(t, f.m, reviewer, "POST", "/review/verifications/"+v.ID+"/decision", map[string]any{"status": "rejected", "version": 1}), 409)
	w = call(t, f.m, reviewer, "PATCH", "/review/verifications/"+v.ID, verificationInput{RealName: v.RealName, StudentID: v.StudentID, IdentityTitle: "更新后的身份", IdentityType: "management", Version: v.Version})
	status(t, w, 200)
	v = result[Verification](t, w)
	profile := result[Member](t, call(t, f.m, alice, "GET", "/profile", nil))
	if profile.IdentityLevel != 4 || profile.IdentityTitle != "更新后的身份" {
		t.Fatal("approved edit did not synchronize member")
	}
	var stored []byte
	if e := f.rt.DB.QueryRow("SELECT data FROM eid_verifications WHERE id=?", v.ID).Scan(&stored); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(stored, []byte("Private Alice")) || bytes.Contains(stored, []byte("private-student")) {
		t.Fatal("PII is not encrypted")
	}
	f.rt.DB.Close()
	f.open(t)
	w = call(t, f.m, alice, "GET", "/verifications", nil)
	status(t, w, 200)
	items := result[struct {
		Items []Verification `json:"items"`
	}](t, w).Items
	if len(items) != 1 || len(items[0].History) != 3 {
		t.Fatalf("state/history did not persist: %+v", items)
	}
	status(t, call(t, f.m, reviewer, "POST", "/review/verifications/"+v.ID+"/decision", map[string]any{"status": "rejected", "version": v.Version}), 200)
	if result[Member](t, call(t, f.m, alice, "GET", "/profile", nil)).IdentityLevel != 0 {
		t.Fatal("rejected credential remains effective")
	}
	if len(result[struct {
		Items []Verification `json:"items"`
	}](t, call(t, f.m, alice, "GET", "/cards", nil)).Items) != 0 {
		t.Fatal("rejected card remains visible")
	}
	noScope := httptest.NewRecorder()
	f.m.Handler().ServeHTTP(noScope, httptest.NewRequest("GET", "/profile", nil))
	status(t, noScope, 401)
	spoof := actor("alice", "read", "write")
	spoof.ApplicationID = "trust"
	status(t, call(t, f.m, spoof, "GET", "/profile", nil), 404)
}
func trustedActor(t *testing.T, f *testFixture, s appkit.Scope) (trust.Scheme, trust.Submission) {
	t.Helper()
	admin := actor("trust-admin", "read", "write", "admin", "review")
	admin.ApplicationID = "trust"
	admin.InstallationID = "trust-a"
	w := call(t, f.trust, admin, "POST", "/schemes", trust.Scheme{Name: "入团资格", Status: "active", Fields: []trust.Field{{Name: "name", Label: "姓名", Type: "text", Required: true}}})
	status(t, w, 201)
	scheme := result[trust.Scheme](t, w)
	ts := s
	ts.ApplicationID = "trust"
	ts.InstallationID = "trust-a"
	w = call(t, f.trust, ts, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "data": map[string]string{"name": "Alice"}})
	status(t, w, 201)
	sub := result[trust.Submission](t, w)
	w = call(t, f.trust, admin, "POST", "/review/submissions/"+sub.ID, map[string]any{"status": "approved", "version": sub.Version})
	status(t, w, 200)
	sub = result[trust.Submission](t, w)
	return scheme, sub
}
func TestClubTrustGateAndPersonalOffer(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	bob := actor("bob", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	scheme, _ := trustedActor(t, f, alice)
	cfg := defaults()
	cfg.TrustSchemeID = scheme.ID
	cfg.SMTPAddress = "mail.example.test:465"
	cfg.SMTPFrom = "recruit@example.test"
	saveCfg(t, f, cfg)
	status(t, call(t, f.m, bob, "POST", "/club/applications", map[string]any{"realName": "Bob", "external_verified": true}), 400)
	status(t, call(t, f.m, bob, "POST", "/club/applications", map[string]any{"realName": "Bob"}), 403)
	f.enabled = false
	status(t, call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"}), 403)
	f.enabled = true
	w := call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"})
	status(t, w, 201)
	a := result[ClubApplication](t, w)
	if !a.TrustVerified {
		t.Fatal("server trust proof missing")
	}
	status(t, call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"}), 409)
	status(t, call(t, f.m, actor("owner", "read", "manage", "admin"), "GET", "/review/club", nil), 403)
	status(t, call(t, f.m, bob, "POST", "/club/applications/"+a.ID+"/confirm", map[string]any{"version": a.Version}), 404)
	if len(result[struct {
		Items []ClubApplication `json:"items"`
	}](t, call(t, f.m, bob, "GET", "/club", nil)).Items) != 0 {
		t.Fatal("other user's recruitment leaked")
	}
	sent := 0
	f.m.mailer = func(_ context.Context, _ Settings, to, subject, body string) error {
		sent++
		if to != alice.Email || !strings.Contains(body, "tenantId=team-a") || !strings.Contains(body, "tab=club") {
			t.Fatal("wrong recipient or link")
		}
		return nil
	}
	send := func(kind, key string, resend bool) *httptest.ResponseRecorder {
		return call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", map[string]any{"kind": kind, "resend": resend, "version": a.Version, "idempotencyKey": key})
	}
	status(t, send("offer", "early-offer", false), 409)
	w = send("interview", "first-interview", false)
	status(t, w, 200)
	if result[Notification](t, w).State != "sent" {
		t.Fatal(w.Body.String())
	}
	status(t, send("interview", "first-interview", false), 200)
	if sent != 1 {
		t.Fatal("idempotent replay delivered duplicate mail")
	}
	a, _ = f.m.club(context.Background(), alice, a.ID)
	w = send("offer", "first-offer", false)
	status(t, w, 200)
	a, _ = f.m.club(context.Background(), alice, a.ID)
	status(t, call(t, f.m, bob, "POST", "/club/applications/"+a.ID+"/confirm", map[string]any{"version": a.Version}), 404)
	status(t, call(t, f.m, alice, "POST", "/club/applications/"+a.ID+"/confirm", map[string]any{"version": a.Version}), 200)
	a, _ = f.m.club(context.Background(), alice, a.ID)
	if a.Status != "offer_confirmed" {
		t.Fatal(a.Status)
	}
	status(t, send("offer", "after-confirm", true), 409)
	status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/decision", map[string]any{"status": "rejected", "version": a.Version}), 409)
}
func TestNotificationsFailureNeverAdvancesAndSettingsSecretBoundary(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	cfg := defaults()
	cfg.RequireTrust = false
	saveCfg(t, f, cfg)
	w := call(t, f.m, alice, "POST", "/club/applications", map[string]any{"realName": "Alice"})
	status(t, w, 201)
	a := result[ClubApplication](t, w)
	body := map[string]any{"kind": "interview", "version": a.Version, "idempotencyKey": "try-1"}
	status(t, call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", body), 503)
	cfg.SMTPAddress = "smtp.example.test:465"
	cfg.SMTPFrom = "sender@example.test"
	cfg.SMTPPassword = "private-smtp-pass"
	saveCfg(t, f, cfg)
	f.m.mailer = func(context.Context, Settings, string, string, string) error {
		return errors.New("test failure: secret-must-not-leak")
	}
	w = call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", body)
	status(t, w, 200)
	if result[Notification](t, w).State != "failed" || strings.Contains(w.Body.String(), "secret-must-not-leak") {
		t.Fatal(w.Body.String())
	}
	updated, _ := f.m.club(context.Background(), alice, a.ID)
	if updated.Status != "pending" {
		t.Fatal("failure advanced application")
	}
	f.m.mailer = func(context.Context, Settings, string, string, string) error {
		return &uncertainDelivery{errors.New("ack lost")}
	}
	body["idempotencyKey"] = "try-2"
	w = call(t, f.m, reviewer, "POST", "/review/club/"+a.ID+"/notifications", body)
	status(t, w, 200)
	if result[Notification](t, w).State != "uncertain" {
		t.Fatal(w.Body.String())
	}
	owner := actor("owner", "read", "manage")
	w = call(t, f.m, owner, "GET", "/settings", nil)
	status(t, w, 200)
	if strings.Contains(w.Body.String(), "private-smtp-pass") {
		t.Fatal("credential leaked")
	}
	cfg.SMTPPassword = "new-password"
	status(t, call(t, f.m, owner, "PUT", "/settings", cfg), 403)
	foreign := reviewer
	foreign.ProjectID = "other"
	foreign.InstallationID = "eid-other"
	status(t, call(t, f.m, foreign, "POST", "/review/club/"+a.ID+"/notifications", body), 404)
}
func TestAtomicAuditFailureAndConcurrentApplication(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	if _, e := f.rt.DB.Exec("DROP TABLE audit"); e != nil {
		t.Fatal(e)
	}
	status(t, call(t, f.m, alice, "POST", "/verifications", verificationInput{RealName: "Alice", StudentID: "42", IdentityTitle: "core", IdentityType: "core"}), 500)
	var n int
	f.rt.DB.QueryRow("SELECT COUNT(*) FROM eid_verifications").Scan(&n)
	if n != 0 {
		t.Fatal("application survived missing audit transaction")
	}
	if _, e := f.rt.DB.Exec(`CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER)`); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call(t, f.m, alice, "POST", "/verifications", verificationInput{RealName: "Alice", StudentID: "42", IdentityTitle: "core", IdentityType: "core"})
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent applications: %+v", counts)
	}
}
func TestSMTPActualTLSDelivery(t *testing.T) {
	f := fixture(t)
	t.Setenv("EULER_EID_ALLOW_LOCAL_SMTP", "true")
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificates := certificateServer.TLS.Certificates
	pool := x509.NewCertPool()
	pool.AddCert(certificateServer.Certificate())
	certificateServer.Close()
	listener, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	messages := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			errs <- e
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		p := textproto.NewConn(conn)
		p.PrintfLine("220 local SMTP ready")
		for {
			line, e := p.ReadLine()
			if e != nil {
				errs <- e
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				p.PrintfLine("250 local")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				p.PrintfLine("250 OK")
			case line == "DATA":
				p.PrintfLine("354 continue")
				b, e := p.ReadDotBytes()
				if e != nil {
					errs <- e
					return
				}
				messages <- string(b)
				p.PrintfLine("250 accepted")
			case line == "QUIT":
				p.PrintfLine("221 goodbye")
				errs <- nil
				return
			default:
				errs <- errors.New("unexpected SMTP command")
				return
			}
		}
	}()
	f.m.smtpTLSConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	cfg := defaults()
	cfg.SMTPAddress = listener.Addr().String()
	cfg.SMTPFrom = "sender@example.test"
	cfg.SMTPImplicitTLS = true
	if e = f.m.sendSMTP(context.Background(), cfg, "alice@example.test", "测试录取", "你好，Alice\n请接受录取"); e != nil {
		t.Fatal(e)
	}
	if e = <-errs; e != nil {
		t.Fatal(e)
	}
	message := <-messages
	if !strings.Contains(message, "To: alice@example.test") || !strings.Contains(message, "请接受录取") || !strings.Contains(message, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatal(message)
	}
	t.Setenv("EULER_EID_ALLOW_LOCAL_SMTP", "")
	if e = f.m.sendSMTP(context.Background(), cfg, "alice@example.test", "test", "body"); e == nil {
		t.Fatal("private SMTP must require deployment opt-in")
	}
}

func TestQualificationKeysAreBoundAndRevocable(t *testing.T) {
	f := fixture(t)
	alice := actor("alice", "read", "write")
	reviewer := actor("reviewer", "read", "review")
	admin := actor("operator", "read", "admin", "secrets")
	v := apply(t, f, alice)
	status(t, call(t, f.m, reviewer, "POST", "/review/verifications/"+v.ID+"/decision", map[string]any{"status": "approved", "version": v.Version}), 200)
	input := map[string]any{"name": "Admission integration", "actorIds": []string{alice.ActorID}, "expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339)}
	status(t, call(t, f.m, actor("owner", "read", "manage", "secrets"), "POST", "/keys", input), 403)
	status(t, call(t, f.m, actor("operator", "read", "admin"), "POST", "/keys", input), 403)
	w := call(t, f.m, admin, "POST", "/keys", input)
	status(t, w, 201)
	created := result[struct {
		Key   QueryKey `json:"key"`
		Token string   `json:"token"`
	}](t, w)
	query := func(actorID, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/verification/status?actorId="+actorID, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		f.m.PublicHandler().ServeHTTP(w, r)
		return w
	}
	status(t, query("alice", ""), 401)
	w = query("alice", created.Token)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), "approved") || strings.Contains(w.Body.String(), "Private Alice") || strings.Contains(w.Body.String(), "private-student") {
		t.Fatal("wrong qualification disclosure", w.Body.String())
	}
	status(t, query("bob", created.Token), 404)
	var hash string
	if e := f.rt.DB.QueryRow("SELECT token_hash FROM eid_query_keys WHERE id=?", created.Key.ID).Scan(&hash); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(hash, created.Token) {
		t.Fatal("query credential stored plaintext")
	}
	f.enabled = false
	status(t, query("alice", created.Token), 404)
	f.enabled = true
	foreign := admin
	foreign.ProjectID = "project-b"
	foreign.InstallationID = "eid-b"
	status(t, call(t, f.m, foreign, "DELETE", "/keys/"+created.Key.ID, nil), 404)
	status(t, call(t, f.m, admin, "DELETE", "/keys/"+created.Key.ID, nil), 204)
	status(t, query("alice", created.Token), 403)
}
