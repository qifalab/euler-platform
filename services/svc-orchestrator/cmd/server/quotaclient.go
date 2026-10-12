package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// quotaURLEnv locates svc-quota. Unset outside production, quota is not
// enforced (main logs it once); production refuses to start without it.
const quotaURLEnv = "EULER_SVC_QUOTA_URL"

// quotaReserver is svc-quota's two-phase reservation protocol as the
// fulfilment saga uses it (03§4.3.2): occupy before provisioning, commit once
// the resource is RUNNING, release when the provisioning is compensated, and
// free the committed capacity when the resource is released.
type quotaReserver interface {
	// Occupy reserves capacity. svc-quota records req.IdempotencyKey with
	// the reservation, durably: every later occupy with that key — on any
	// replica, after any restart — replays the same reservation, and reports
	// whether it is still active (pending). A key whose reservation ended
	// (released, swept) never reserves again; the caller moves to a new key
	// (occupyActive). errQuotaExceeded when the account has no room;
	// errQuotaUnmodelled when svc-quota has no quota for the product.
	Occupy(ctx context.Context, accountID int64, req quotaOccupyRequest) (quotaReservation, error)
	// Commit turns the reservation into used capacity. errReservationGone
	// when svc-quota no longer has it (already committed, released, or
	// expired and swept).
	Commit(ctx context.Context, accountID int64, reservationID string) error
	// Release returns an uncommitted reservation; releasing an unknown one
	// succeeds (saga compensation).
	Release(ctx context.Context, accountID int64, reservationID string) error
	// Free gives a released resource's committed capacity back
	// (POST /internal/quota/free). Idempotent by req.IdempotencyKey: a retry
	// applies once. errQuotaUnmodelled when svc-quota has no such quota.
	Free(ctx context.Context, accountID int64, req quotaFreeRequest) error
}

// quotaReservation is an occupy answer.
type quotaReservation struct {
	ID string
	// Active is false when the answer replays a reservation that has since
	// ended (committed, released or swept).
	Active bool
}

// quotaFreeRequest is svc-quota's POST /internal/quota/free body.
type quotaFreeRequest struct {
	ProductCode    string `json:"productCode"`
	ResourceType   string `json:"resourceType"`
	Region         string `json:"region"`
	Amount         int    `json:"amount"`
	BizKey         string `json:"bizKey"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// quotaOccupyRequest is svc-quota's POST /api/v1/quota/occupy body.
type quotaOccupyRequest struct {
	ProductCode    string `json:"productCode"`
	ResourceType   string `json:"resourceType"`
	Region         string `json:"region"`
	Count          int    `json:"count"`
	BizKey         string `json:"bizKey"`
	IdempotencyKey string `json:"idempotencyKey"`
}

var (
	errQuotaExceeded   = errors.New("quota exceeded")
	errQuotaUnmodelled = errors.New("svc-quota has no quota for this product")
	errReservationGone = errors.New("quota reservation no longer exists")
)

// Reservation bookkeeping values of attrQuotaState.
const (
	quotaOccupied  = "OCCUPIED"  // reserved, not yet committed
	quotaCommitted = "COMMITTED" // counted as used
	quotaReleased  = "RELEASED"  // reservation returned (never delivered)
	quotaFreed     = "FREED"     // used capacity returned (resource released)
)

// httpQuotaClient calls svc-quota with the caller's account id and the
// internal token, like the svc-order client.
type httpQuotaClient struct {
	base  string
	http  *http.Client
	token string
}

// newQuotaReserverFromEnv returns the svc-quota client, or nil when
// EULER_SVC_QUOTA_URL is unset (quota not enforced — main refuses that in
// production).
func newQuotaReserverFromEnv() quotaReserver {
	base := strings.TrimSpace(os.Getenv(quotaURLEnv))
	if base == "" {
		return nil
	}
	return &httpQuotaClient{
		base:  strings.TrimRight(base, "/"),
		http:  &http.Client{Timeout: 5 * time.Second},
		token: os.Getenv("EULER_INTERNAL_TOKEN"),
	}
}

// quotaEnvelope is svc-quota's response envelope.
type quotaEnvelope struct {
	Code    string          `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

func (c *httpQuotaClient) post(ctx context.Context, accountID int64, path string, body any) (int, quotaEnvelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, quotaEnvelope{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, quotaEnvelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	if c.token != "" {
		req.Header.Set("X-Euler-Internal-Token", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, quotaEnvelope{}, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, quotaEnvelope{}, err
	}
	var env quotaEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return resp.StatusCode, quotaEnvelope{}, fmt.Errorf("svc-quota %s: HTTP %d, undecodable body: %w", path, resp.StatusCode, err)
	}
	return resp.StatusCode, env, nil
}

func (c *httpQuotaClient) Occupy(ctx context.Context, accountID int64, r quotaOccupyRequest) (quotaReservation, error) {
	status, env, err := c.post(ctx, accountID, "/api/v1/quota/occupy", r)
	if err != nil {
		return quotaReservation{}, err
	}
	switch {
	case status == http.StatusOK && env.Code == "OK":
		var data struct {
			ReservationID string `json:"reservationId"`
			Replayed      bool   `json:"replayed"`
			Active        *bool  `json:"active"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil || data.ReservationID == "" {
			return quotaReservation{}, fmt.Errorf("svc-quota occupy: no reservation id in %s", env.Data)
		}
		// A build that predates `active` only answers live reservations.
		active := data.Active == nil || *data.Active
		return quotaReservation{ID: data.ReservationID, Active: active}, nil
	case env.Code == "Quota.Exceeded":
		return quotaReservation{}, fmt.Errorf("%w: %s", errQuotaExceeded, env.Message)
	case status == http.StatusNotFound && env.Code == "Quota.UnknownQuota":
		return quotaReservation{}, errQuotaUnmodelled
	}
	return quotaReservation{}, fmt.Errorf("svc-quota occupy: HTTP %d %s %s", status, env.Code, env.Message)
}

func (c *httpQuotaClient) Free(ctx context.Context, accountID int64, r quotaFreeRequest) error {
	status, env, err := c.post(ctx, accountID, "/internal/quota/free", r)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusOK && env.Code == "OK":
		return nil
	case status == http.StatusNotFound && env.Code == "Quota.UnknownQuota":
		return errQuotaUnmodelled
	}
	return fmt.Errorf("svc-quota free: HTTP %d %s %s", status, env.Code, env.Message)
}

func (c *httpQuotaClient) Commit(ctx context.Context, accountID int64, reservationID string) error {
	status, env, err := c.post(ctx, accountID, "/api/v1/quota/commit", map[string]string{"reservationId": reservationID})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusOK && env.Code == "OK":
		return nil
	case env.Code == "Quota.NotFound" || env.Code == "Quota.ReservationExpired":
		return fmt.Errorf("%w: %s %s", errReservationGone, env.Code, env.Message)
	}
	return fmt.Errorf("svc-quota commit: HTTP %d %s %s", status, env.Code, env.Message)
}

func (c *httpQuotaClient) Release(ctx context.Context, accountID int64, reservationID string) error {
	status, env, err := c.post(ctx, accountID, "/api/v1/quota/release", map[string]string{"reservationId": reservationID})
	if err != nil {
		return err
	}
	if status == http.StatusOK && env.Code == "OK" {
		return nil
	}
	return fmt.Errorf("svc-quota release: HTTP %d %s %s", status, env.Code, env.Message)
}
