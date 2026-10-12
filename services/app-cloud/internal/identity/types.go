// Package identity owns Euler's independent OIDC login and browser sessions.
// EID remains a separate member application; none of its authentication is changed.
package identity

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("identity record not found")

// ExternalIdentity comes from strict OIDC validation or the explicitly selected
// OAuth2 protected-UserInfo flow. Both bind a one-use state to PKCE. Never join
// identities by an email or a display name.
type ExternalIdentity struct {
	Provider      string `json:"provider"`
	Subject       string `json:"subject"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	EmailVerified bool   `json:"emailVerified"`
}

type Principal struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Subject       string `json:"subject"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	EmailVerified bool   `json:"emailVerified"`
}

// Session contains a digest of the bearer cookie, never the cookie value itself.
// The independent CSRF token can be returned by the same-origin session API.
type Session struct {
	TokenHash string
	UserID    string
	CSRFToken string
	ExpiresAt time.Time
}

// AuthFlow is short lived and consumed atomically before exchanging a code.
// Persistent implementations must protect Verifier and clean up expired flows.
type AuthFlow struct {
	StateHash string
	Nonce     string
	Verifier  string
	ReturnTo  string
	ExpiresAt time.Time
}

type SessionStore interface {
	UpsertIdentity(context.Context, ExternalIdentity) (Principal, error)
	GetPrincipal(context.Context, string) (Principal, error)
	PutSession(context.Context, Session) error
	GetSession(context.Context, string) (Session, error)
	DeleteSession(context.Context, string) error
	PutAuthFlow(context.Context, AuthFlow) error
	ConsumeAuthFlow(context.Context, string) (AuthFlow, error)
}
