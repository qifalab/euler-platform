package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// Typed calls on the internal REST services. Paths and JSON shapes are the
// services' own (services/svc-catalog, svc-order, svc-orchestrator,
// svc-monitor cmd/server handlers); every call is scoped to the caller's
// account by downstream.do.

// --- svc-catalog -------------------------------------------------------------

// skuStatusOnSale is t_sku.status of a sellable SKU (svc-catalog).
const skuStatusOnSale = "1"

// Charge types as svc-catalog / svc-order spell them (pkg-go/pricing).
const (
	chargePrepaid  = "PREPAID"
	chargePostpaid = "POSTPAID"
)

// catalogSKU is one row of GET /api/v1/catalog/skus.
type catalogSKU struct {
	SKUCode     string `json:"skuCode"`
	ProductCode string `json:"productCode"`
	ChargeType  string `json:"chargeType"`
	Status      string `json:"status"`
}

// catalogRegion is one row of GET /api/v1/catalog/regions.
type catalogRegion struct {
	RegionID string `json:"regionId"`
	Zones    []struct {
		ZoneID string `json:"zoneId"`
	} `json:"zones"`
}

func (s *server) skus(c *call) ([]catalogSKU, error) {
	var out []catalogSKU
	_, err := s.catalog.do(c, http.MethodGet, "/api/v1/catalog/skus", url.Values{"productCode": {c.product}}, nil, &out)
	return out, err
}

func (s *server) regions(c *call) ([]catalogRegion, error) {
	var out []catalogRegion
	_, err := s.catalog.do(c, http.MethodGet, "/api/v1/catalog/regions", nil, nil, &out)
	return out, err
}

// --- svc-order ---------------------------------------------------------------

// Order states (pkg-go/order) the purchase flow branches on.
const (
	orderPendingPayment = "PENDING_PAYMENT"
	orderCancelled      = "CANCELLED"
)

// orderRecord is svc-order's order view (orderToMap).
type orderRecord struct {
	OrderID     int64  `json:"orderId"`
	OrderNo     string `json:"orderNo"`
	State       string `json:"state"`
	ProductCode string `json:"productCode"`
	SKUCode     string `json:"skuCode"`
	RegionID    string `json:"regionId"`
	ZoneID      string `json:"zoneId"`
	ChargeType  string `json:"chargeType"`
	Duration    int64  `json:"duration"`
}

// createOrderBody is POST /api/v1/orders. svc-order prices it itself and
// returns the existing order for a clientToken it has seen (per account).
type createOrderBody struct {
	Type        string `json:"type"`
	ProductCode string `json:"productCode"`
	SKUCode     string `json:"skuCode"`
	RegionID    string `json:"regionId"`
	ZoneID      string `json:"zoneId,omitempty"`
	ChargeType  string `json:"chargeType"`
	Quantity    int64  `json:"quantity"`
	Duration    int64  `json:"duration"`
	ClientToken string `json:"clientToken,omitempty"`
}

func (s *server) createOrder(c *call, b createOrderBody) (*orderRecord, error) {
	var o orderRecord
	if _, err := s.order.do(c, http.MethodPost, "/api/v1/orders", nil, b, &o); err != nil {
		return nil, err
	}
	if o.OrderID <= 0 {
		return nil, contractError(s.order, "order create returned no orderId")
	}
	return &o, nil
}

// payOrder pays an order from the account balance. svc-order makes a second
// pay of a settled order an idempotent no-op (never a second debit).
func (s *server) payOrder(c *call, orderID int64) error {
	_, err := s.order.do(c, http.MethodPost, "/api/v1/orders/"+strconv.FormatInt(orderID, 10)+"/pay", nil, struct{}{}, nil)
	return err
}

// --- svc-orchestrator --------------------------------------------------------

// Resource lifecycle states (pkg-go/resource) the facade branches on.
const (
	stateCreating = "CREATING"
	stateReleased = "RELEASED"
)

// fulfilBody is POST /api/v1/orchestrator/fulfill. The orchestrator takes
// product, spec, region, zone and charge type from the PAID order (the body
// may only repeat them); params become the resource's attributes.
type fulfilBody struct {
	OrderID     int64             `json:"orderId"`
	OrderNo     string            `json:"orderNo,omitempty"`
	ProductCode string            `json:"productCode"`
	Region      string            `json:"region"`
	ZoneID      string            `json:"zoneId,omitempty"`
	SpecCode    string            `json:"specCode"`
	ChargeType  string            `json:"chargeType"`
	Params      map[string]string `json:"params,omitempty"`
}

// fulfilResult is the fulfil answer: 200 delivered, 202 still provisioning
// (state CREATING). A replay for the same order returns the same resource.
type fulfilResult struct {
	ResourceID string `json:"resourceId"`
	State      string `json:"state"`
	OrderID    int64  `json:"orderId"`
}

// resourceRecord is the orchestrator's resource detail/list record.
type resourceRecord struct {
	ResourceID  string            `json:"resourceId"`
	ProductCode string            `json:"productCode"`
	Region      string            `json:"region"`
	Zone        string            `json:"zone"`
	ChargeType  string            `json:"chargeType"`
	State       string            `json:"state"`
	SpecCode    string            `json:"specCode"`
	ExpiredAt   string            `json:"expiredAt"`
	CreatedAt   string            `json:"createdAt"`
	Attributes  map[string]string `json:"attributes"`
}

// resourceIDPattern bounds a resource id before it is put in a path. Anything
// else cannot name a resource, so it is answered as not found.
var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (s *server) fulfil(c *call, b fulfilBody) (*fulfilResult, error) {
	var out fulfilResult
	if _, err := s.orchestrator.do(c, http.MethodPost, "/api/v1/orchestrator/fulfill", nil, b, &out); err != nil {
		return nil, err
	}
	if out.ResourceID == "" {
		return nil, contractError(s.orchestrator, "fulfil returned no resourceId")
	}
	return &out, nil
}

// listResources returns the caller's live resources of the calling product.
// The product filter is applied here too: the orchestrator lists every
// product of the account.
func (s *server) listResources(c *call) ([]resourceRecord, error) {
	var all []resourceRecord
	if _, err := s.orchestrator.do(c, http.MethodGet, "/api/v1/orchestrator/resources",
		url.Values{"product": {c.product}}, nil, &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if r.ProductCode == c.product && r.State != stateReleased {
			out = append(out, r)
		}
	}
	return out, nil
}

// getResource reads one resource of the caller (any product); the
// orchestrator answers 404 Resource.NotFound for another account's.
func (s *server) getResource(c *call, id string) (*resourceRecord, error) {
	if !resourceIDPattern.MatchString(id) {
		return nil, &downstreamError{Service: s.orchestrator.name, Status: http.StatusNotFound, Code: "Resource.NotFound"}
	}
	var r resourceRecord
	if _, err := s.orchestrator.do(c, http.MethodGet, "/api/v1/orchestrator/resources/"+url.PathEscape(id), nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// resource returns one live resource of the calling product owned by the
// caller, or the product's not-found error: a resource of another account
// (the orchestrator scopes by account), of another product (an euvpc id
// handed to StopInstance) or one already released all read as "does not
// exist".
func (s *server) resource(c *call, id string) (*resourceRecord, error) {
	r, err := s.getResource(c, id)
	if err != nil {
		if downstreamCode(err) == "Resource.NotFound" {
			return nil, c.notFound()
		}
		return nil, err
	}
	if r.ResourceID != id || r.ProductCode != c.product || r.State == stateReleased {
		return nil, c.notFound()
	}
	return r, nil
}

// resourceAction applies start|stop|restart|upgrade and returns the new state.
func (s *server) resourceAction(c *call, id, action, specCode string) (string, error) {
	body := struct {
		Action   string `json:"action"`
		SpecCode string `json:"specCode,omitempty"`
	}{action, specCode}
	var out struct {
		State string `json:"state"`
	}
	_, err := s.orchestrator.do(c, http.MethodPost, "/api/v1/orchestrator/resources/"+url.PathEscape(id)+"/actions", nil, body, &out)
	return out.State, err
}

func (s *server) releaseResource(c *call, id string) error {
	_, err := s.orchestrator.do(c, http.MethodPost, "/api/v1/orchestrator/resources/"+url.PathEscape(id)+"/release", nil, struct{}{}, nil)
	return err
}

// patchAttributes changes mutable attributes (name) and returns the detail.
func (s *server) patchAttributes(c *call, id string, attrs map[string]string) (*resourceRecord, error) {
	body := struct {
		Attributes map[string]string `json:"attributes"`
	}{attrs}
	var r resourceRecord
	if _, err := s.orchestrator.do(c, http.MethodPatch, "/api/v1/orchestrator/resources/"+url.PathEscape(id)+"/attributes", nil, body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// --- svc-monitor -------------------------------------------------------------

// alarmRule is svc-monitor's AlertRule.
type alarmRule struct {
	RuleID               int64    `json:"ruleId"`
	ProductCode          string   `json:"productCode"`
	ResourceType         string   `json:"resourceType"`
	Metric               string   `json:"metric"`
	Threshold            string   `json:"threshold"`
	ComparisonOperator   int      `json:"comparisonOperator"`
	Period               int      `json:"period"`
	EvalPeriods          int      `json:"evalPeriods"`
	NotificationChannels []string `json:"notificationChannels"`
	Status               int      `json:"status"`
	Version              int      `json:"version"`
	CreatedAt            string   `json:"createdAt"`
	UpdatedAt            string   `json:"updatedAt"`
}

// metricSeries is GET /api/v1/monitor/metrics (svc-monitor metrics.go):
// {metric, resourceId, start, end, period, source, points:[{timestamp, value}]}.
type metricSeries struct {
	Metric     string `json:"metric"`
	ResourceID string `json:"resourceId"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Period     int64  `json:"period"` // seconds
	Source     string `json:"source"` // prometheus | synthetic
	Points     []struct {
		Timestamp string      `json:"timestamp"`
		Value     json.Number `json:"value"`
	} `json:"points"`
}

func (s *server) alarmRules(c *call) ([]alarmRule, error) {
	var out []alarmRule
	_, err := s.monitor.do(c, http.MethodGet, "/api/v1/monitor/rules", nil, nil, &out)
	return out, err
}

func (s *server) postAlarmRule(c *call, body any) (*alarmRule, error) {
	var out alarmRule
	if _, err := s.monitor.do(c, http.MethodPost, "/api/v1/monitor/rules", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *server) updateAlarmRule(c *call, id int64, body any) (*alarmRule, error) {
	var out alarmRule
	if _, err := s.monitor.do(c, http.MethodPut, "/api/v1/monitor/rules/"+strconv.FormatInt(id, 10), nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *server) deleteAlarmRule(c *call, id int64) error {
	_, err := s.monitor.do(c, http.MethodDelete, "/api/v1/monitor/rules/"+strconv.FormatInt(id, 10), nil, nil, nil)
	return err
}

func (s *server) metricData(c *call, q url.Values) (*metricSeries, error) {
	var out metricSeries
	if _, err := s.monitor.do(c, http.MethodGet, "/api/v1/monitor/metrics", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// contractError is a 2xx answer that breaks the documented contract: an
// unavailability, never a caller error.
func contractError(d *downstream, msg string) error {
	return &downstreamError{Service: d.name, Status: http.StatusOK, Err: errors.New(msg)}
}
