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

// paidOrder is what svc-order says about an order. Fulfilment provisions
// exactly this — never the product/spec/region the fulfil request claims.
type paidOrder struct {
	OrderID     int64
	OrderNo     string
	State       string
	ProductCode string
	SKUCode     string
	RegionID    string
	// ZoneID is the availability zone the order was placed for (svc-order's
	// zoneId); "" for orders without one (older builds, REGIONAL products).
	ZoneID     string
	ChargeType string
	Payable    pricing.Amount
	// The purchased term (svc-order's quantity/duration/durationUnit). Older
	// svc-order builds do not send them: zero values mean "unknown", and a
	// prepaid resource is then created without an ExpiredAt rather than with
	// an invented one.
	Quantity     int64
	Duration     int64
	DurationUnit pricing.DurationUnit
}

// errOrderNotFound means svc-order has no such order for this account.
var errOrderNotFound = errors.New("svc-orchestrator: order not found")

// orderSource reads the caller's order from svc-order. Fulfilment used to
// fabricate a PAID mirror for any order id it had not seen, so a caller could
// provision resources for orders that were never created or paid.
type orderSource interface {
	Order(ctx context.Context, accountID, orderID int64) (paidOrder, error)
}

// fulfilmentReporter tells svc-order how a paid order's provisioning saga
// settled (POST /internal/orders/{id}/fulfilment). svc-order owns the order;
// without this report it kept every order PAID and never refunded a failed one.
type fulfilmentReporter interface {
	ReportFulfilment(ctx context.Context, accountID, orderID int64, rep fulfilmentReport) error
}

// fulfilmentReport is the body svc-order's fulfilment endpoint takes.
type fulfilmentReport struct {
	Result     string `json:"result"` // fulfilmentCompleted | fulfilmentFailed
	ResourceID string `json:"resourceId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

const (
	fulfilmentCompleted = "COMPLETED"
	fulfilmentFailed    = "FAILED"
)

// httpOrderSource calls svc-order GET /api/v1/orders/{id} with the caller's
// account id (svc-order scopes the lookup by account, so another tenant's order
// is simply not found).
type httpOrderSource struct {
	base  string
	http  *http.Client
	token string
}

func newHTTPOrderSource() httpOrderSource {
	base := strings.TrimSpace(os.Getenv("EULER_SVC_ORDER_URL"))
	if base == "" {
		base = "http://localhost:9204"
	}
	return httpOrderSource{
		base:  strings.TrimRight(base, "/"),
		http:  &http.Client{Timeout: 5 * time.Second},
		token: os.Getenv("EULER_INTERNAL_TOKEN"),
	}
}

func (s httpOrderSource) Order(ctx context.Context, accountID, orderID int64) (paidOrder, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.base+"/api/v1/orders/"+strconv.FormatInt(orderID, 10), nil)
	if err != nil {
		return paidOrder{}, err
	}
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	if s.token != "" {
		req.Header.Set("X-Euler-Internal-Token", s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return paidOrder{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return paidOrder{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return paidOrder{}, errOrderNotFound
	}
	var env struct {
		Code string `json:"Code"`
		Data struct {
			OrderID       int64  `json:"orderId"`
			OrderNo       string `json:"orderNo"`
			State         string `json:"state"`
			ProductCode   string `json:"productCode"`
			SKUCode       string `json:"skuCode"`
			RegionID      string `json:"regionId"`
			ZoneID        string `json:"zoneId"`
			ChargeType    string `json:"chargeType"`
			PayableAmount string `json:"payableAmount"`
			Quantity      int64  `json:"quantity"`
			Duration      int64  `json:"duration"`
			DurationUnit  string `json:"durationUnit"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return paidOrder{}, fmt.Errorf("order lookup: decode: %w", err)
	}
	if resp.StatusCode != http.StatusOK || env.Code != "OK" {
		return paidOrder{}, fmt.Errorf("order lookup: HTTP %d %s", resp.StatusCode, env.Code)
	}
	if env.Data.OrderID != orderID {
		return paidOrder{}, fmt.Errorf("order lookup: asked for %d, got %d", orderID, env.Data.OrderID)
	}
	payable, err := pricing.ParseAmount(env.Data.PayableAmount)
	if err != nil {
		return paidOrder{}, fmt.Errorf("order lookup: payable %q: %w", env.Data.PayableAmount, err)
	}
	return paidOrder{
		OrderID: env.Data.OrderID, OrderNo: env.Data.OrderNo, State: env.Data.State,
		ProductCode: env.Data.ProductCode, SKUCode: env.Data.SKUCode, RegionID: env.Data.RegionID,
		ZoneID: strings.TrimSpace(env.Data.ZoneID), ChargeType: env.Data.ChargeType, Payable: payable,
		Quantity: env.Data.Quantity, Duration: env.Data.Duration,
		DurationUnit: pricing.DurationUnit(strings.ToUpper(strings.TrimSpace(env.Data.DurationUnit))),
	}, nil
}

// ReportFulfilment posts the saga's outcome to svc-order. svc-order applies it
// idempotently; anything but 200 OK is an error the caller logs and a fulfil
// replay re-sends (503 Order.RefundPending included: the refund is retried).
func (s httpOrderSource) ReportFulfilment(ctx context.Context, accountID, orderID int64, rep fulfilmentReport) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.base+"/internal/orders/"+strconv.FormatInt(orderID, 10)+"/fulfilment", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	if s.token != "" {
		req.Header.Set("X-Euler-Internal-Token", s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode != http.StatusOK || env.Code != "OK" {
		return fmt.Errorf("fulfilment report: HTTP %d %s %s", resp.StatusCode, env.Code, env.Message)
	}
	return nil
}
