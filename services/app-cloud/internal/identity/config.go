package identity

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config belongs to Euler alone. No EID client, session or database is reused.
// Empty credentials disable login at the caller; they never enable a fake user.
type Config struct {
	Mode                    string // oidc (default) or explicitly oauth2_userinfo
	Issuer                  string
	ClientID                string
	ClientSecret            string
	PublicOrigin            string
	RedirectURL             string
	SessionTTL              time.Duration
	FlowTTL                 time.Duration
	TokenEndpointAuthMethod string // client_secret_basic (default) or client_secret_post
	AllowInsecureLoopback   bool   // only explicit local development / test issuers
	HTTPClient              *http.Client
	AuthorizationEndpoint   string // required only in oauth2_userinfo mode
	TokenEndpoint           string
	UserInfoEndpoint        string
	UserInfoSubjectClaim    string // sub (default) or id; from the protected UserInfo response
	UserInfoEnvelope        string // empty (standard) or data (explicit legacy response wrapper)
}

func (c Config) validate() (Config, error) {
	if c.Mode == "" {
		c.Mode = "oidc"
	}
	if c.Mode != "oidc" && c.Mode != "oauth2_userinfo" {
		return c, errors.New("identity mode must be oidc or oauth2_userinfo")
	}
	if strings.TrimSpace(c.Issuer) == "" || strings.TrimSpace(c.ClientID) == "" || strings.TrimSpace(c.ClientSecret) == "" {
		return c, errors.New("OIDC issuer, client ID and client secret must be configured")
	}
	issuer, err := endpointURL(c.Issuer, c.AllowInsecureLoopback)
	if err != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return c, errors.New("OIDC issuer must be an HTTPS URL without query or fragment")
	}
	// Preserve issuer spelling: OIDC issuer comparison must remain exact.
	origin, err := endpointURL(c.PublicOrigin, c.AllowInsecureLoopback)
	if err != nil || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return c, errors.New("public origin must be an HTTPS origin")
	}
	c.PublicOrigin = origin.Scheme + "://" + origin.Host
	callback, err := endpointURL(c.RedirectURL, c.AllowInsecureLoopback)
	if err != nil || callback.Scheme+"://"+callback.Host != c.PublicOrigin || callback.RawQuery != "" || callback.Fragment != "" || callback.Path == "" {
		return c, errors.New("OIDC callback must be a fixed path on the public origin")
	}
	if c.TokenEndpointAuthMethod == "" {
		c.TokenEndpointAuthMethod = "client_secret_basic"
	}
	if c.TokenEndpointAuthMethod != "client_secret_basic" && c.TokenEndpointAuthMethod != "client_secret_post" {
		return c, errors.New("unsupported token endpoint authentication method")
	}
	if c.UserInfoSubjectClaim == "" {
		c.UserInfoSubjectClaim = "sub"
	}
	if c.UserInfoSubjectClaim != "sub" && c.UserInfoSubjectClaim != "id" {
		return c, errors.New("userinfo subject claim must be sub or id")
	}
	if c.UserInfoEnvelope != "" && c.UserInfoEnvelope != "data" {
		return c, errors.New("userinfo envelope must be empty or data")
	}
	if c.Mode == "oidc" && (c.UserInfoSubjectClaim != "sub" || c.UserInfoEnvelope != "") {
		return c, errors.New("OIDC requires standard sub claims")
	}
	if c.Mode == "oauth2_userinfo" {
		for _, raw := range []string{c.AuthorizationEndpoint, c.TokenEndpoint, c.UserInfoEndpoint} {
			if _, err := endpointURL(raw, c.AllowInsecureLoopback); err != nil {
				return c, errors.New("OAuth2 mode requires fixed secure authorization, token and userinfo endpoints")
			}
		}
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 8 * time.Hour
	}
	if c.FlowTTL == 0 {
		c.FlowTTL = 10 * time.Minute
	}
	if c.SessionTTL < time.Minute || c.SessionTTL > 24*time.Hour || c.FlowTTL < time.Minute || c.FlowTTL > 10*time.Minute {
		return c, errors.New("invalid session or login lifetime")
	}
	return c, nil
}

func endpointURL(raw string, allowLoopback bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return nil, errors.New("invalid endpoint URL")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	ip := net.ParseIP(u.Hostname())
	if allowLoopback && u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return u, nil
	}
	return nil, errors.New("endpoint must use HTTPS")
}

func safeReturnTo(raw string) string {
	if raw == "" {
		return "/"
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "\\") {
		return "/"
	}
	for _, ch := range u.Path {
		if ch < 32 || ch == 127 {
			return "/"
		}
	}
	return raw
}
