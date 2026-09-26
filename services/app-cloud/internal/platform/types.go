// Package platform owns the persistent tenant and application control plane.
package platform

import (
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("resource not found")
	ErrForbidden = errors.New("permission denied")
	ErrConflict  = errors.New("operation conflicts with existing state")
	ErrInvalid   = errors.New("invalid request")
)

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}
type Member struct {
	UserID      string    `json:"userId"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joinedAt"`
}
type Project struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenantId"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}
type Invitation struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type Installation struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenantId"`
	ProjectID     string          `json:"projectId"`
	ApplicationID string          `json:"applicationId"`
	Status        string          `json:"status"`
	CreatedAt     time.Time       `json:"createdAt"`
	Connection    *ConnectionInfo `json:"connection,omitempty"`
}
type ConnectionInfo struct {
	BaseURL    string    `json:"baseUrl"`
	Configured bool      `json:"configured"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Connection is server-only. Its credential must never be JSON encoded.
type Connection struct {
	BaseURL           string
	Credential        string `json:"-"`
	ExternalAccountID string
}
type AuditEvent struct {
	ID        string    `json:"id"`
	ActorID   string    `json:"actorId"`
	Action    string    `json:"action"`
	TargetID  string    `json:"targetId"`
	ProjectID string    `json:"projectId"`
	CreatedAt time.Time `json:"createdAt"`
	Summary   string    `json:"summary"`
}
type Resource struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status,omitempty"`
	URL    string `json:"url,omitempty"`
}
