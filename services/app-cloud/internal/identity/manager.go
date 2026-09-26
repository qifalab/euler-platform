package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrCSRF            = errors.New("same-origin CSRF validation failed")
)

// Manager never accepts caller-supplied user IDs, roles, or bearer ID tokens.
// Identity comes only from our one-time code exchange with the configured source.
type Manager struct {
	config         Config
	store          SessionStore
	client         *http.Client
	oauth          oauth2.Config
	verifier       *oidc.IDTokenVerifier
	userinfo       string
	secure         bool
	cookieName     string
	flowCookieName string
	now            func() time.Time
}

func New(ctx context.Context, config Config, store SessionStore) (*Manager, error) {
	c, err := config.validate()
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("identity store is required")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if c.HTTPClient != nil {
		*client = *c.HTTPClient
		if client.Timeout == 0 {
			client.Timeout = 15 * time.Second
		}
	}
	// Redirects must never forward a code, client credential or access token to
	// another endpoint. Public endpoint URLs come from trusted configuration.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if t, ok := client.Transport.(*http.Transport); ok && t.TLSClientConfig != nil && t.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("identity TLS certificate verification cannot be disabled")
	}
	m := &Manager{config: c, store: store, client: client, secure: strings.HasPrefix(c.PublicOrigin, "https://"), now: time.Now}
	m.cookieName, m.flowCookieName = "euler_session", "euler_oidc_state"
	if m.secure {
		m.cookieName, m.flowCookieName = "__Host-euler_session", "__Host-euler_oidc_state"
	}
	endpoint := oauth2.Endpoint{AuthURL: c.AuthorizationEndpoint, TokenURL: c.TokenEndpoint}
	m.userinfo = c.UserInfoEndpoint
	if c.Mode == "oidc" {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), c.Issuer)
		if err != nil {
			return nil, errors.New("OIDC discovery unavailable or issuer mismatch")
		}
		var metadata struct {
			Issuer     string   `json:"issuer"`
			JWKS       string   `json:"jwks_uri"`
			UserInfo   string   `json:"userinfo_endpoint"`
			Algorithms []string `json:"id_token_signing_alg_values_supported"`
		}
		if provider.Claims(&metadata) != nil || metadata.Issuer != c.Issuer {
			return nil, errors.New("invalid OIDC discovery")
		}
		endpoint = provider.Endpoint()
		m.userinfo = metadata.UserInfo
		for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKS} {
			if _, err := endpointURL(raw, c.AllowInsecureLoopback); err != nil {
				return nil, errors.New("OIDC discovery contains insecure endpoints")
			}
		}
		if m.userinfo != "" {
			if _, err := endpointURL(m.userinfo, c.AllowInsecureLoopback); err != nil {
				return nil, errors.New("OIDC userinfo endpoint must be secure")
			}
		}
		allowed := []string{}
		for _, alg := range metadata.Algorithms {
			switch alg {
			case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA":
				allowed = append(allowed, alg)
			}
		}
		if len(allowed) == 0 {
			return nil, errors.New("OIDC issuer must advertise a verifiable asymmetric signing algorithm")
		}
		m.verifier = provider.VerifierContext(oidc.ClientContext(context.Background(), client), &oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: allowed})
	}
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	if c.TokenEndpointAuthMethod == "client_secret_post" {
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	m.oauth = oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: endpoint, RedirectURL: c.RedirectURL, Scopes: []string{"openid", "profile", "email"}}
	return m, nil
}

func (m *Manager) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", m.Login)
	mux.HandleFunc("GET /auth/callback", m.Callback)
	mux.HandleFunc("POST /auth/logout", m.Logout)
}

func privateResponse(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	privateResponse(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func opaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is also useful to a persistence implementation's expiry cleanup.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func validOpaque(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32 && len(token) == 43
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (m *Manager) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds()), Expires: m.now().Add(ttl)})
}

func (m *Manager) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}

func uniqueCookie(r *http.Request, name string) (string, error) {
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			value = c.Value
			count++
		}
	}
	if count != 1 || !validOpaque(value) {
		return "", ErrUnauthenticated
	}
	return value, nil
}

func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	privateResponse(w)
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "请使用 GET 发起登录")
		return
	}
	state, err := opaqueToken()
	if err != nil {
		writeError(w, 500, "login_unavailable", "暂时无法发起登录")
		return
	}
	nonce, err := opaqueToken()
	if err != nil {
		writeError(w, 500, "login_unavailable", "暂时无法发起登录")
		return
	}
	verifier := oauth2.GenerateVerifier()
	flow := AuthFlow{StateHash: HashToken(state), Nonce: nonce, Verifier: verifier, ReturnTo: safeReturnTo(r.URL.Query().Get("returnTo")), ExpiresAt: m.now().Add(m.config.FlowTTL)}
	if err := m.store.PutAuthFlow(r.Context(), flow); err != nil {
		writeError(w, 503, "login_unavailable", "登录存储暂时不可用")
		return
	}
	m.setCookie(w, m.flowCookieName, state, m.config.FlowTTL)
	// PKCE is mandatory in both explicit modes. nonce is checked in OIDC mode.
	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if m.config.Mode == "oidc" {
		options = append(options, oidc.Nonce(nonce))
	}
	http.Redirect(w, r, m.oauth.AuthCodeURL(state, options...), http.StatusFound)
}

func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	privateResponse(w)
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "请使用 GET 返回登录结果")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q["state"]) != 1 || len(q["code"]) > 1 || len(q["error"]) > 1 || q.Has("id_token") || q.Has("access_token") {
		writeError(w, 400, "invalid_callback", "登录回调参数无效")
		return
	}
	state := q.Get("state")
	cookie, err := uniqueCookie(r, m.flowCookieName)
	if err != nil || !validOpaque(state) || !same(cookie, state) {
		writeError(w, 400, "invalid_state", "登录状态无效，请重新登录")
		return
	}
	m.clearCookie(w, m.flowCookieName)
	flow, err := m.store.ConsumeAuthFlow(r.Context(), HashToken(state))
	if err != nil || !flow.ExpiresAt.After(m.now()) {
		writeError(w, 400, "expired_login", "登录已过期或已使用，请重新登录")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		writeError(w, 400, "login_cancelled", "登录未完成，请重试")
		return
	}
	// Only this back-channel exchange can produce a principal. Provider errors,
	// token bodies and secrets must never be reflected in a browser error.
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, m.client)
	token, err := m.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		writeError(w, 502, "identity_unavailable", "身份服务未能完成授权")
		return
	}
	external, err := m.resolveIdentity(ctx, token, flow.Nonce)
	if err != nil {
		writeError(w, 401, "identity_rejected", "无法验证登录身份，请重新登录")
		return
	}
	principal, err := m.store.UpsertIdentity(r.Context(), external)
	if err != nil || principal.ID == "" {
		writeError(w, 503, "session_unavailable", "无法建立登录会话")
		return
	}
	value, err := opaqueToken()
	if err != nil {
		writeError(w, 500, "session_unavailable", "无法建立登录会话")
		return
	}
	csrf, err := opaqueToken()
	if err != nil {
		writeError(w, 500, "session_unavailable", "无法建立登录会话")
		return
	}
	// Successful reauthentication rotates the browser's old bearer session.
	if old, err := uniqueCookie(r, m.cookieName); err == nil {
		if err := m.store.DeleteSession(r.Context(), HashToken(old)); err != nil && !errors.Is(err, ErrNotFound) {
			writeError(w, 503, "session_unavailable", "无法更新登录会话")
			return
		}
	}
	session := Session{TokenHash: HashToken(value), UserID: principal.ID, CSRFToken: csrf, ExpiresAt: m.now().Add(m.config.SessionTTL)}
	if err := m.store.PutSession(r.Context(), session); err != nil {
		writeError(w, 503, "session_unavailable", "无法建立登录会话")
		return
	}
	m.setCookie(w, m.cookieName, value, m.config.SessionTTL)
	http.Redirect(w, r, safeReturnTo(flow.ReturnTo), http.StatusSeeOther)
}

func (m *Manager) Authenticate(r *http.Request) (Principal, Session, error) {
	value, err := uniqueCookie(r, m.cookieName)
	if err != nil {
		return Principal{}, Session{}, ErrUnauthenticated
	}
	session, err := m.store.GetSession(r.Context(), HashToken(value))
	if err != nil || !session.ExpiresAt.After(m.now()) || session.UserID == "" {
		return Principal{}, Session{}, ErrUnauthenticated
	}
	principal, err := m.store.GetPrincipal(r.Context(), session.UserID)
	if err != nil || principal.ID == "" {
		return Principal{}, Session{}, ErrUnauthenticated
	}
	return principal, session, nil
}

// ValidateCSRF requires both a same-origin browser request and the independent
// session token returned by GET /api/v1/session. No trust in forwarding headers.
func (m *Manager) ValidateCSRF(r *http.Request, session Session) error {
	if r.Header.Get("Origin") != m.config.PublicOrigin || r.Header.Get("Sec-Fetch-Site") == "cross-site" || !validOpaque(session.CSRFToken) || !same(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		return ErrCSRF
	}
	return nil
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	privateResponse(w)
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "请使用 POST 退出登录")
		return
	}
	_, session, err := m.Authenticate(r)
	if err != nil {
		writeError(w, 401, "unauthenticated", "请先登录")
		return
	}
	if m.ValidateCSRF(r, session) != nil {
		writeError(w, 403, "csrf_rejected", "请求来源验证失败")
		return
	}
	if err := m.store.DeleteSession(r.Context(), session.TokenHash); err != nil && !errors.Is(err, ErrNotFound) {
		writeError(w, 503, "logout_unavailable", "暂时无法退出，请重试")
		return
	}
	m.clearCookie(w, m.cookieName)
	m.clearCookie(w, m.flowCookieName)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) resolveIdentity(ctx context.Context, token *oauth2.Token, nonce string) (ExternalIdentity, error) {
	if m.config.Mode == "oauth2_userinfo" {
		// Explicit OAuth2 mode trusts the protected UserInfo response only. An
		// id_token, if returned, is never decoded or used for identity/permissions.
		return m.fetchUserInfo(ctx, token.AccessToken, m.config.UserInfoSubjectClaim, m.config.UserInfoEnvelope)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return ExternalIdentity{}, errors.New("missing ID token")
	}
	idToken, err := m.verifier.Verify(ctx, raw)
	if err != nil {
		return ExternalIdentity{}, errors.New("ID token validation failed")
	}
	var claims struct {
		Exp               *int64 `json:"exp"`
		Iat               *int64 `json:"iat"`
		Nbf               *int64 `json:"nbf"`
		AZP               string `json:"azp"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
	}
	if idToken.Claims(&claims) != nil || claims.Exp == nil || claims.Iat == nil || strings.TrimSpace(idToken.Subject) == "" || !same(idToken.Nonce, nonce) {
		return ExternalIdentity{}, errors.New("required ID token claim missing or invalid")
	}
	now := m.now().Unix()
	if *claims.Exp <= now || *claims.Iat > now+60 || *claims.Iat < now-600 || *claims.Exp <= *claims.Iat || (claims.Nbf != nil && *claims.Nbf > now+60) {
		return ExternalIdentity{}, errors.New("ID token time claims invalid")
	}
	if (len(idToken.Audience) > 1 && claims.AZP != m.config.ClientID) || (claims.AZP != "" && claims.AZP != m.config.ClientID) {
		return ExternalIdentity{}, errors.New("ID token authorized party mismatch")
	}
	if idToken.AccessTokenHash != "" && idToken.VerifyAccessToken(token.AccessToken) != nil {
		return ExternalIdentity{}, errors.New("access token hash mismatch")
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	result := ExternalIdentity{Provider: m.config.Issuer, Subject: idToken.Subject, Name: name, Email: claims.Email, EmailVerified: claims.EmailVerified}
	if m.userinfo != "" {
		profile, err := m.fetchUserInfo(ctx, token.AccessToken, "sub", "")
		if err != nil || profile.Subject != idToken.Subject {
			return ExternalIdentity{}, errors.New("userinfo subject mismatch or unavailable")
		}
		if profile.Name != "" {
			result.Name = profile.Name
		}
		if profile.Email != "" {
			result.Email = profile.Email
			result.EmailVerified = profile.EmailVerified
		}
	}
	return result, nil
}

func (m *Manager) fetchUserInfo(ctx context.Context, accessToken, subjectClaim, envelope string) (ExternalIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.userinfo, nil)
	if err != nil {
		return ExternalIdentity{}, errors.New("userinfo request invalid")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return ExternalIdentity{}, errors.New("userinfo unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExternalIdentity{}, errors.New("userinfo rejected")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return ExternalIdentity{}, errors.New("userinfo response too large")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(body, &data) != nil {
		return ExternalIdentity{}, errors.New("userinfo response invalid")
	}
	if envelope == "data" {
		var code int
		if json.Unmarshal(data["code"], &code) != nil || code != 200 {
			return ExternalIdentity{}, errors.New("userinfo envelope unsuccessful")
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(data["data"], &nested) != nil {
			return ExternalIdentity{}, errors.New("userinfo envelope invalid")
		}
		data = nested
	}
	var subject string
	if json.Unmarshal(data[subjectClaim], &subject) != nil && subjectClaim == "id" {
		// Preserve integer IDs lexically, avoiding float64 precision loss.
		candidate := string(data[subjectClaim])
		valid := candidate != ""
		for _, ch := range candidate {
			if ch < '0' || ch > '9' {
				valid = false
			}
		}
		if valid {
			subject = candidate
		}
	}
	if strings.TrimSpace(subject) == "" || len(subject) > 1024 {
		return ExternalIdentity{}, errors.New("userinfo subject missing")
	}
	var name, email string
	var verified bool
	for _, field := range []string{"name", "preferred_username", "username"} {
		if json.Unmarshal(data[field], &name) == nil && name != "" {
			break
		}
	}
	if v, ok := data["email"]; ok && json.Unmarshal(v, &email) != nil {
		return ExternalIdentity{}, errors.New("userinfo email invalid")
	}
	if v, ok := data["email_verified"]; ok && json.Unmarshal(v, &verified) != nil {
		return ExternalIdentity{}, errors.New("userinfo email verification invalid")
	}
	return ExternalIdentity{Provider: m.config.Issuer, Subject: subject, Name: name, Email: email, EmailVerified: verified}, nil
}
