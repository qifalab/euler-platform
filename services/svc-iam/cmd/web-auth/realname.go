package main

// Real-name verification (07§2.3).
//
// F6 — provider: the national-standard checksums (GB 11643 / GB 32100) reject
// made-up numbers but cannot prove a document belongs to the person. With
// EULER_REALNAME_PROVIDER_URL + EULER_REALNAME_PROVIDER_SECRET set, a
// checksum-valid submission is also checked by an external provider:
//
//	POST <url>   {"name": "...", "idNumber": "...", "type": "individual"|"enterprise"}
//	X-Euler-Signature: sha256=<hex HMAC-SHA256(secret, raw body)>
//	→ 200 {"match": true|false}
//
// Fail closed: a timeout, a non-200, a malformed answer → 503
// Realname.ProviderUnavailable, nothing recorded. No match → 400
// Realname.Mismatch (5 per account per day, then 429). Unset: checksum-only,
// as before, announced at startup.
//
// F4 — GET /internal/accounts/{id}/realname: the verified state for other
// services (svc-order's trial flow dedups on identityKey). Internal-token
// only, never routed by the gateway.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/runmode"
)

const (
	realnameProviderURLEnv    = "EULER_REALNAME_PROVIDER_URL"
	realnameProviderSecretEnv = "EULER_REALNAME_PROVIDER_SECRET"
	// realnameSignatureHeader carries "sha256=" + hex HMAC of the body.
	realnameSignatureHeader = "X-Euler-Signature"
	realnameProviderTimeout = 5 * time.Second
)

// realnameCheck is what a provider is asked: the submitted name (person or
// enterprise) and document number (resident ID or credit code).
type realnameCheck struct {
	Name     string `json:"name"`
	IDNumber string `json:"idNumber"`
	Type     string `json:"type"` // individual | enterprise
}

// realnameVerifier decides whether a checksum-valid submission is genuine.
type realnameVerifier interface {
	Verify(ctx context.Context, c realnameCheck) (match bool, err error)
}

// realnameProvider is the configured verifier (configureIntegrations).
var realnameProvider realnameVerifier = checksumOnlyVerifier{}

// checksumOnlyVerifier accepts what passed the checksum validation — the
// behaviour without a provider.
type checksumOnlyVerifier struct{}

func (checksumOnlyVerifier) Verify(context.Context, realnameCheck) (bool, error) { return true, nil }

// httpRealnameProvider asks an external verification service.
type httpRealnameProvider struct {
	url    string
	secret []byte
	client *http.Client
}

var errProviderAnswer = errors.New("realname provider: malformed answer")

func (p httpRealnameProvider) Verify(ctx context.Context, c realnameCheck) (bool, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	mac := hmac.New(sha256.New, p.secret)
	mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(realnameSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("realname provider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("realname provider: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Match *bool `json:"match"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.Match == nil {
		return false, errProviderAnswer
	}
	return *out.Match, nil
}

// realnameProviderFromEnv builds the verifier. A URL without a secret (or the
// reverse) is a configuration error, and production requires https.
func realnameProviderFromEnv() (realnameVerifier, error) {
	raw := strings.TrimSpace(os.Getenv(realnameProviderURLEnv))
	secret := os.Getenv(realnameProviderSecretEnv)
	if raw == "" {
		if secret != "" {
			return nil, fmt.Errorf("%s is set without %s", realnameProviderSecretEnv, realnameProviderURLEnv)
		}
		slog.Warn("no real-name verification provider configured — documents are checked by checksum only (GB 11643 / GB 32100)", "set", realnameProviderURLEnv)
		return checksumOnlyVerifier{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%s must be an http(s) URL", realnameProviderURLEnv)
	}
	if u.Scheme != "https" && runmode.Production() {
		return nil, fmt.Errorf("%s must use https in production (it carries ID numbers)", realnameProviderURLEnv)
	}
	if secret == "" {
		return nil, fmt.Errorf("%s is required with %s (requests are HMAC-signed)", realnameProviderSecretEnv, realnameProviderURLEnv)
	}
	slog.Info("real-name verification provider configured", "host", u.Host)
	return httpRealnameProvider{url: raw, secret: []byte(secret), client: &http.Client{Timeout: realnameProviderTimeout}}, nil
}

// realnameMismatchLimiter caps provider mismatches per account (every call
// costs money and an unbounded loop is an identity-guessing oracle).
var realnameMismatchLimiter = newAttemptLimiter(5, 24*time.Hour, 24*time.Hour)

// verifyWithProvider runs the configured verifier, answering the request on
// failure. It never logs the name or the document number.
func verifyWithProvider(w http.ResponseWriter, r *http.Request, accountID int64, c realnameCheck) bool {
	key := "realname:" + strconv.FormatInt(accountID, 10)
	now := time.Now()
	if realnameMismatchLimiter.Locked(key, now) {
		writeErr(w, 429, "Auth.TooManyAttempts", "实名核验失败次数过多,请 24 小时后再试")
		return false
	}
	match, err := realnameProvider.Verify(r.Context(), c)
	if err != nil {
		slog.Error("real-name provider unavailable", "account_id", accountID, "type", c.Type, "err", err)
		writeErr(w, 503, "Realname.ProviderUnavailable", "实名核验服务暂不可用,请稍后再试")
		return false
	}
	if !match {
		realnameMismatchLimiter.Fail(key, now)
		writeErr(w, 400, "Realname.Mismatch", "实名信息核验不一致,请确认姓名与证件号")
		return false
	}
	return true
}

// handleInternalRealname implements GET /internal/accounts/{id}/realname:
//
//	200 {"verified": bool, "type": "personal"|"enterprise"|"", "identityKey": "<hex HMAC>"|""}
//	404 Auth.AccountNotFound
//
// identityKey is the HMAC identity fingerprint (EULER_IDENTITY_PEPPER) — equal
// for two accounts verified with the same document, and never reversible to
// the number.
func handleInternalRealname(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "Common.InvalidParameter", "invalid account id")
		return
	}
	a, err := st.AccountByID(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, 404, "Auth.AccountNotFound", "account not found")
		return
	}
	if err != nil {
		internalError(w, r, "internal realname lookup", err)
		return
	}
	typ, key := "", ""
	switch a.RealNameStatus {
	case 1:
		typ, key = "personal", a.IdentityFingerprint
	case 2:
		typ, key = "enterprise", a.IdentityFingerprint
	}
	writeJSON(w, 200, "OK", map[string]any{"verified": typ != "", "type": typ, "identityKey": key})
}
