// Package connectors talks only to explicitly configured product origins.
// Callers must authorize the Euler project before supplying its connection.
package connectors

import (
	"fmt"
	"time"
)

type Config struct {
	AllowedOrigins       []string
	AllowInsecureHTTP    bool // Deployment setting, never accepted from a binding request.
	AllowPrivateNetwork  bool // Required for explicitly allowlisted private deployments.
	Timeout              time.Duration
	VerificationProvider string // Exact verified identity provider; subjects are not interchangeable.
	EIDBaseURL           string
	TrustBaseURL         string
	TrustSchemeID        string
	ConsoleURLs          map[string]string
}

type Connection struct {
	BaseURL           string `json:"baseUrl"`
	Credential        string `json:"-"`
	ExternalAccountID string `json:"externalAccountId,omitempty"`
}

// Credential describes the upstream's actual credential. A user bearer token
// is not a scoped Euler service credential. Storage keys are account-wide too.
type Credential struct {
	Kind      string `json:"kind"`
	Token     string `json:"token,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
}

type Subject struct{ Provider, ID string }

type Application struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Category       string   `json:"category"`
	Description    string   `json:"description"`
	Color          string   `json:"color"`
	Homepage       string   `json:"homepage"`
	Repository     string   `json:"repository"`
	Capabilities   []string `json:"capabilities"`
	ConnectionMode string   `json:"connectionMode"`
	Limitations    []string `json:"limitations"`
}

type Metric struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type Verification struct {
	Status       string `json:"status"`
	IdentityType string `json:"identityType,omitempty"`
}
type Summary struct {
	State        string        `json:"state"`
	Message      string        `json:"message"`
	Metrics      []Metric      `json:"metrics,omitempty"`
	Verification *Verification `json:"verification,omitempty"`
	ConsoleURL   string        `json:"consoleUrl,omitempty"`
	CheckedAt    time.Time     `json:"checkedAt"`
}
type Resource struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status,omitempty"`
	URL    string `json:"url,omitempty"`
}
type CreateResource struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
}

// Error deliberately excludes upstream bodies, URL query strings and credentials.
type Error struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	StatusCode     int    `json:"-"`
	UpstreamStatus int    `json:"-"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func failure(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, StatusCode: status}
}
