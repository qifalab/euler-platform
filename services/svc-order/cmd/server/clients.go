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

	"github.com/qifalab/euler-platform/pricing"
)

// --- downstream ports ----------------------------------------------------------
//
// svc-order never takes money or price from the caller. The payable amount of an
// order comes from svc-catalog's pricing engine (quoter), and marking an order
// PAID requires svc-payment to actually debit the account's balance (payer).
// Before this, the console sent amountMinor itself and "paid" by POSTing a
// made-up paymentId, so any tenant could open resources for free.

// quoteInput is what svc-order asks the pricing engine to price.
type quoteInput struct {
	ProductCode string
	SKUCode     string
	ChargeType  string // PREPAID / POSTPAID; "" = the SKU's catalogue default
	RegionID    string
	ZoneID      string
	Quantity    int64
	Duration    int64
}

// quoteResult is the engine's verdict for one order line.
type quoteResult struct {
	Payable      pricing.Amount
	ChargeType   pricing.ChargeType
	DurationUnit pricing.DurationUnit
	// RegionScope is the product's placement scope (REGIONAL | ZONAL) when the
	// catalogue reports it with the quote; "" when it does not. svc-catalog
	// already refuses a ZONAL quote without a valid zone in the region
	// (Catalog.ZoneRequired / InvalidZone / ZoneRegionMismatch); the scope
	// lets svc-order also drop a zone a REGIONAL product would ignore.
	RegionScope string
}

type quoter interface {
	Quote(ctx context.Context, accountID int64, in quoteInput) (quoteResult, error)
}

// payer debits the caller's cash balance for an order. idemKey makes a retried
// payment collapse onto the first debit instead of charging twice.
type payer interface {
	Consume(ctx context.Context, accountID int64, amountMinor int64, idemKey, bizKey, reason string) (entryID string, err error)
	// Refund gives back the debit made under consumeKey (amountMinor must be
	// exactly what it took). It is idempotent, and refunded=false means no debit
	// exists under the key — there is nothing to give back.
	Refund(ctx context.Context, accountID int64, consumeKey string, amountMinor int64, reason string) (refunded bool, err error)
}

// errInsufficientBalance is the payer's "not enough money" verdict.
var errInsufficientBalance = errors.New("svc-order: insufficient balance")

// errPaymentConflict means the order's payment key already names a different
// ledger movement. That is never proof of payment: the order stays unpaid.
var errPaymentConflict = errors.New("svc-order: payment key holds a different ledger entry")

// downstreamError is a 4xx verdict from a downstream service, surfaced to the
// caller with the downstream's own error code (e.g. Catalog.SKUNotFound).
type downstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *downstreamError) Error() string { return e.Code + ": " + e.Message }

// --- HTTP implementations ------------------------------------------------------

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// internalClient calls sibling services with the caller's account id and, when
// configured, the shared internal token (so it keeps working once
// EULER_INTERNAL_TOKEN is switched on).
type internalClient struct {
	base  string
	http  *http.Client
	token string
}

func newInternalClient(base string) internalClient {
	return internalClient{
		base:  strings.TrimRight(base, "/"),
		http:  &http.Client{Timeout: 5 * time.Second},
		token: os.Getenv("EULER_INTERNAL_TOKEN"),
	}
}

type envelope struct {
	Code    string          `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

func (c internalClient) post(ctx context.Context, path string, accountID int64, body any) (int, envelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, envelope{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, envelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	if c.token != "" {
		req.Header.Set(internalTokenHeader, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, envelope{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, envelope{}, err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return resp.StatusCode, envelope{}, fmt.Errorf("decode %s response: %w", path, err)
	}
	return resp.StatusCode, env, nil
}

// catalogQuoter prices through svc-catalog POST /api/v1/catalog/quote.
type catalogQuoter struct{ internalClient }

func newCatalogQuoter() catalogQuoter {
	return catalogQuoter{newInternalClient(envOrDefault("EULER_SVC_CATALOG_URL", "http://localhost:9207"))}
}

func (c catalogQuoter) Quote(ctx context.Context, accountID int64, in quoteInput) (quoteResult, error) {
	// customerLevel / userTag are deliberately NOT forwarded: they select
	// discounts and the caller cannot be trusted to name its own tier.
	status, env, err := c.post(ctx, "/api/v1/catalog/quote", accountID, map[string]any{
		"productCode": in.ProductCode,
		"specCode":    in.SKUCode,
		"chargeType":  in.ChargeType,
		"duration":    in.Duration,
		"quantity":    in.Quantity,
		"regionId":    in.RegionID,
		"zoneId":      in.ZoneID,
	})
	if err != nil {
		return quoteResult{}, err
	}
	if status != http.StatusOK || env.Code != "OK" {
		if status >= 400 && status < 500 && env.Code != "" {
			return quoteResult{}, &downstreamError{Status: status, Code: env.Code, Message: env.Message}
		}
		return quoteResult{}, fmt.Errorf("catalog quote: HTTP %d %s", status, env.Code)
	}
	var d struct {
		PayableAmount string `json:"payableAmount"`
		ChargeType    string `json:"chargeType"`
		DurationUnit  string `json:"durationUnit"`
		RegionScope   string `json:"regionScope"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return quoteResult{}, fmt.Errorf("catalog quote: decode data: %w", err)
	}
	payable, err := pricing.ParseAmount(d.PayableAmount)
	if err != nil {
		return quoteResult{}, fmt.Errorf("catalog quote: payableAmount %q: %w", d.PayableAmount, err)
	}
	return quoteResult{
		Payable:      payable,
		ChargeType:   pricing.ChargeType(d.ChargeType),
		DurationUnit: pricing.DurationUnit(d.DurationUnit),
		RegionScope:  d.RegionScope,
	}, nil
}

// balancePayer debits through svc-payment's internal POST
// /internal/payment/consume. The internal route only accepts keys in the
// reserved namespace (order-pay-{id}) that the tenant-facing routes refuse, so
// an entry found under an order's key was written by svc-order itself.
type balancePayer struct{ internalClient }

func newBalancePayer() balancePayer {
	return balancePayer{newInternalClient(envOrDefault("EULER_SVC_PAYMENT_URL", "http://localhost:9205"))}
}

func (p balancePayer) Consume(ctx context.Context, accountID int64, amountMinor int64, idemKey, bizKey, reason string) (string, error) {
	status, env, err := p.post(ctx, "/internal/payment/consume", accountID, map[string]any{
		"amountMinor":    amountMinor,
		"idempotencyKey": idemKey,
		"bizKey":         bizKey,
		"reason":         reason,
	})
	if err != nil {
		return "", err
	}
	if env.Code == "Payment.InsufficientBalance" {
		return "", errInsufficientBalance
	}
	if env.Code == "Payment.IdempotencyKeyConflict" {
		return "", errPaymentConflict
	}
	if status != http.StatusOK || env.Code != "OK" {
		return "", fmt.Errorf("payment consume: HTTP %d %s %s", status, env.Code, env.Message)
	}
	var d struct {
		EntryID     json.Number `json:"entryId"`
		Type        string      `json:"type"`
		AmountMinor json.Number `json:"amountMinor"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil || d.EntryID == "" || d.EntryID == "0" {
		return "", fmt.Errorf("payment consume: response carries no entryId")
	}
	// An entry id alone is not proof of THIS payment: the entry under the key
	// must be a CONSUME of exactly the order's payable. Anything else (an
	// idempotent replay onto a different movement) leaves the order unpaid.
	if got, err := d.AmountMinor.Int64(); err != nil || d.Type != "CONSUME" || got != amountMinor {
		return "", fmt.Errorf("%w: entry %s is %s %s 分, order expects CONSUME %d 分",
			errPaymentConflict, d.EntryID, d.Type, d.AmountMinor, amountMinor)
	}
	return d.EntryID.String(), nil
}

// Refund reverses an order's debit through svc-payment's internal POST
// /internal/payment/refund (idempotent on the consume key).
func (p balancePayer) Refund(ctx context.Context, accountID int64, consumeKey string, amountMinor int64, reason string) (bool, error) {
	status, env, err := p.post(ctx, "/internal/payment/refund", accountID, map[string]any{
		"consumeKey":  consumeKey,
		"amountMinor": amountMinor,
		"reason":      reason,
	})
	if err != nil {
		return false, err
	}
	if status != http.StatusOK || env.Code != "OK" {
		return false, fmt.Errorf("payment refund: HTTP %d %s %s", status, env.Code, env.Message)
	}
	var d struct {
		Refunded bool `json:"refunded"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return false, fmt.Errorf("payment refund: decode data: %w", err)
	}
	return d.Refunded, nil
}

// roundToCent rounds a non-negative micro-yuan amount half-up to whole 分. The
// order tables and the balance ledger settle in 分, so the payable is fixed at
// cent precision when the order is created — never re-rounded later.
func roundToCent(a pricing.Amount) pricing.Amount {
	if a <= 0 {
		return 0
	}
	return (a + 5_000) / 10_000 * 10_000
}

// --- real-name status (svc-iam) -------------------------------------------------

// iamURLEnv points svc-order at svc-iam web-auth, the owner of 实名 status.
const iamURLEnv = "EULER_SVC_IAM_URL"

// realNameStatus is svc-iam's 实名 verdict for an account.
type realNameStatus struct {
	Verified bool
	// Type is "personal", "enterprise", or "" when unverified.
	Type string
	// IdentityKey is the stable hash of the verified document (the raw number
	// never leaves svc-iam); "" when unverified. Trial admission dedups claims
	// across accounts by it, and fails closed when a verified account has none.
	IdentityKey string
}

// realNameSource answers an account's 实名 status.
type realNameSource interface {
	RealName(ctx context.Context, accountID int64) (realNameStatus, error)
}

// iamRealName reads svc-iam web-auth's internal
// GET /internal/accounts/{id}/realname (envelope Data {verified, type,
// identityKey}; 404 for an unknown account) with the internal token.
type iamRealName struct{ internalClient }

func newIAMRealName(base string) iamRealName {
	return iamRealName{newInternalClient(base)}
}

func (c iamRealName) RealName(ctx context.Context, accountID int64) (realNameStatus, error) {
	path := "/internal/accounts/" + strconv.FormatInt(accountID, 10) + "/realname"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return realNameStatus{}, err
	}
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	if c.token != "" {
		req.Header.Set(internalTokenHeader, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return realNameStatus{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return realNameStatus{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// An account svc-iam does not know has verified nothing.
		return realNameStatus{}, nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return realNameStatus{}, fmt.Errorf("iam realname: HTTP %d: decode response: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || env.Code != "OK" {
		return realNameStatus{}, fmt.Errorf("iam realname: HTTP %d %s %s", resp.StatusCode, env.Code, env.Message)
	}
	var d struct {
		Verified    bool   `json:"verified"`
		Type        string `json:"type"`
		IdentityKey string `json:"identityKey"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return realNameStatus{}, fmt.Errorf("iam realname: decode data: %w", err)
	}
	if !d.Verified {
		// An unverified account carries no identity, whatever else came back.
		return realNameStatus{}, nil
	}
	return realNameStatus{Verified: true, Type: d.Type, IdentityKey: d.IdentityKey}, nil
}
