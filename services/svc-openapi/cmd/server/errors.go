package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	errorsx "github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/httpmw"
)

// API error codes. The facade answers in the OpenAPI vocabulary the SDKs and
// the docs use ({Reason}.{Detail}, e.g. InvalidInstanceId.NotFound). A
// downstream service's business verdict is mapped onto it (or passed through
// when it has no OpenAPI name yet — platform codes and messages are
// caller-facing by contract, 03§9.3); a downstream internal failure never is.
const (
	codeMissingParameter       = "MissingParameter"
	codeInvalidParameter       = "InvalidParameter"
	codeInvalidAction          = "InvalidAction.NotFound"
	codeServiceUnavailable     = "ServiceUnavailable"
	codeInternalError          = "InternalError"
	codeInsufficientBalance    = "InsufficientBalance"
	codeQuotaExceeded          = "QuotaExceeded"
	codeIdempotentMismatch     = "IdempotentParameterMismatch"
	codeProvisionFailed        = "ProvisionFailed"
	codeIncorrectOrderStatus   = "IncorrectOrderStatus"
	codeConcurrentModification = "ConcurrentModification"
	codeUpgradeRequiresOrder   = "OperationDenied.UpgradeRequiresOrder"
	codeBucketAlreadyExists    = "BucketAlreadyExists"
	codeInvalidRegion          = "InvalidRegionId.NotFound"
	codeInvalidZone            = "InvalidZoneId.NotFound"
)

var (
	errServiceUnavailable = errorsx.New(codeServiceUnavailable, http.StatusServiceUnavailable,
		"The request has failed due to a temporary failure of the server. Retry the request later.")
	errInternal = errorsx.New(codeInternalError, http.StatusInternalServerError,
		"The request processing has failed due to an unknown error.")
	errProvisionFailed = errorsx.New(codeProvisionFailed, http.StatusConflict,
		"The resource could not be provisioned and its order has been refunded. Retry with a new ClientToken.")
	errOrderNotDeliverable = errorsx.New(codeIncorrectOrderStatus, http.StatusConflict,
		"The order of this request was cancelled or refunded and can no longer be paid or delivered. Retry with a new ClientToken.")
	errIdempotentMismatch = errorsx.New(codeIdempotentMismatch, http.StatusConflict,
		"The ClientToken was already used for a request with different parameters.")
)

func missingParam(name string) *errorsx.Error {
	return errorsx.New(codeMissingParameter, http.StatusBadRequest,
		fmt.Sprintf("The input parameter %s that is mandatory for processing this request is not supplied.", name))
}

func invalidParam(name, reason string) *errorsx.Error {
	return errorsx.New(codeInvalidParameter, http.StatusBadRequest,
		fmt.Sprintf("The specified parameter %s is not valid: %s.", name, reason))
}

// notFound is the product's 404 for a resource the caller cannot see: another
// account's, another product's, released, or never existing — the cases are
// deliberately indistinguishable.
func (c *call) notFound() *errorsx.Error {
	return errorsx.New(c.ps.notFound, http.StatusNotFound,
		fmt.Sprintf("The specified %s does not exist.", c.ps.idParam))
}

func (c *call) incorrectStatus() *errorsx.Error {
	return errorsx.New(c.ps.badStatus, http.StatusConflict,
		"The current status of the resource does not support this operation.")
}

func (c *call) spec() string {
	if c.specParam != "" {
		return c.specParam
	}
	return "SkuCode"
}

func (c *call) specNotFound() *errorsx.Error {
	return errorsx.New("Invalid"+c.spec()+".NotFound", http.StatusBadRequest,
		fmt.Sprintf("The specified %s is not offered for %s.", c.spec(), c.product))
}

func (c *call) specNotOnSale() *errorsx.Error {
	return errorsx.New("Invalid"+c.spec()+".NotOnSale", http.StatusConflict,
		fmt.Sprintf("The specified %s is not on sale.", c.spec()))
}

// invalidAttribute maps the orchestrator's refusal of a creation or modify
// parameter (Resource.InvalidAttribute: a malformed value, a CIDR overlapping
// another VPC, ...). Its message names the attribute and is caller-facing.
func (c *call) invalidAttribute(de *downstreamError) *errorsx.Error {
	code := c.ps.invalidAttr
	if code == "" {
		code = codeInvalidParameter
	}
	status := http.StatusBadRequest
	if de.Status == http.StatusConflict {
		status = http.StatusConflict
	}
	return errorsx.New(code, status, nonEmpty(de.Message, "A parameter is not valid."))
}

// downstreamCode reports the business code of a downstream verdict ("" for
// anything else).
func downstreamCode(err error) string {
	var de *downstreamError
	if errors.As(err, &de) && !de.unavailable() {
		return de.Code
	}
	return ""
}

// translate turns what an action returned into the API error the caller sees.
// API errors pass through. Downstream business verdicts (4xx with a platform
// envelope) are mapped onto the API vocabulary. Everything else — transport
// failures, timeouts, 5xx, responses that break the envelope contract — is
// logged with the trace id and answered with a generic 503 (so the SDKs retry,
// which ClientToken makes safe for creates), never with downstream internals.
func (c *call) translate(err error) *errorsx.Error {
	var apiErr *errorsx.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	rid := httpmw.RequestIDFromContext(c.ctx)
	var de *downstreamError
	if !errors.As(err, &de) {
		slog.Error("openapi action failed", "trace_id", rid, "product", c.product, "action", c.action, "err", err)
		return errInternal
	}
	if de.unavailable() {
		slog.Error("downstream call failed", "trace_id", rid, "product", c.product, "action", c.action,
			"service", de.Service, "status", de.Status, "code", de.Code, "err", de.Error())
		return errServiceUnavailable
	}
	switch code := de.Code; {
	case code == "Resource.NotFound", code == "Monitor.RuleNotFound":
		return c.notFound()
	case code == "Resource.StateTransitionFailed", code == "Resource.InvalidState":
		return c.incorrectStatus()
	case code == "Resource.UpgradeRequiresOrder":
		return errorsx.New(codeUpgradeRequiresOrder, http.StatusConflict,
			"Changing the specification of a Prepaid resource requires an upgrade order, which the OpenAPI does not offer yet.")
	case code == "Resource.InvalidAttribute", code == "Resource.AttributeImmutable":
		return c.invalidAttribute(de)
	case code == "Resource.BucketExists":
		return errorsx.New(codeBucketAlreadyExists, http.StatusConflict,
			"The requested bucket name is not available. Choose a different name.")
	case code == "Order.InsufficientBalance", code == "Payment.InsufficientBalance", code == "Billing.InsufficientBalance":
		return errorsx.New(codeInsufficientBalance, http.StatusForbidden,
			"The account balance is insufficient to pay for this order. Top up and retry with the same ClientToken.")
	case strings.HasPrefix(code, "Quota."):
		return errorsx.New(codeQuotaExceeded, de.Status, "The quota for this resource type is exceeded.")
	case code == "Catalog.SKUNotFound", code == "Catalog.ProductNotFound":
		return c.specNotFound()
	case code == "Catalog.NotOnSale":
		return c.specNotOnSale()
	case code == "Catalog.ChargeTypeMismatch", code == "Order.ChargeTypeNotSupported":
		return invalidParam("ChargeType", "the specification is not sold with this charge type")
	case code == "Catalog.ZoneRequired", code == "Order.ZoneRequired":
		return missingParam("ZoneId")
	case code == "Catalog.InvalidZone", code == "Catalog.ZoneRegionMismatch",
		code == "Order.InvalidZone", code == "Order.ZoneRegionMismatch":
		return invalidParam("ZoneId", "the zone is not available in the region")
	case code == "Catalog.QuoteFailed":
		return errorsx.New(codeInvalidParameter, http.StatusBadRequest,
			"The specification cannot be priced with the given parameters.")
	case code == "Provision.Failed":
		return errProvisionFailed
	case code == "Order.NotPaid", code == "Order.StateTransitionFailed":
		return errOrderNotDeliverable
	case code == "Order.ProductMismatch", code == "Order.SpecMismatch",
		code == "Order.RegionMismatch", code == "Order.ZoneMismatch":
		return errIdempotentMismatch
	case code == "Monitor.VersionConflict":
		return errorsx.New(codeConcurrentModification, http.StatusConflict,
			"The resource was modified concurrently (version mismatch). Read it again and retry.")
	case code == "Common.InvalidParameter":
		return errorsx.New(codeInvalidParameter, http.StatusBadRequest, nonEmpty(de.Message, "A parameter is not valid."))
	case strings.HasPrefix(code, "Common."), code == "Order.NotFound", code == "Order.LineMissing",
		de.Status == http.StatusUnauthorized, de.Status == http.StatusForbidden:
		// Missing account / internal token refused / an order the facade
		// itself just created is unknown: the facade's own wiring is broken,
		// not the caller's request.
		slog.Error("downstream refused the facade", "trace_id", rid, "product", c.product, "action", c.action,
			"service", de.Service, "status", de.Status, "code", de.Code, "message", de.Message)
		return errServiceUnavailable
	default:
		// A business verdict without an OpenAPI name yet (e.g.
		// Order.QuantityUnsupported): platform codes and messages are
		// caller-facing by contract (03§9.3), so it passes through as is.
		return errorsx.New(code, de.Status, nonEmpty(de.Message, code))
	}
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
