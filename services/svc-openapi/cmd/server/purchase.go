package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	errorsx "github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/identifier"
	"github.com/qifalab/euler-platform/topology"
)

// Create actions are paid purchases, run as the console runs them:
//
//	svc-catalog   resolve the SKU (InstanceType/... + ChargeType, or SkuCode)
//	svc-order     create the order under the caller's ClientToken
//	svc-order     pay it from the account balance
//	orchestrator  fulfil it with the creation parameters
//
// Every step is idempotent for a retry with the same ClientToken: svc-order
// returns the order it already holds for the token, paying a settled order is
// a no-op, and the orchestrator returns the resource it already made for the
// order. A retry after a timeout therefore lands on the same resource.

const (
	// maxPeriodMonths is svc-order's longest purchasable term.
	maxPeriodMonths = 60
	// maxClientTokenLen is svc-order's order_main.client_token width.
	maxClientTokenLen = 64
)

var (
	// clientTokenPattern: printable ASCII, no spaces.
	clientTokenPattern = regexp.MustCompile(`^[\x21-\x7e]+$`)
	// specPattern / skuCodePattern bound the spec parts the caller names
	// (s2.large, mysql8.small, essd) and full SKU codes.
	specPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	skuCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// createParams are the purchase parameters every create action shares, plus
// the product-specific attributes handed to the orchestrator.
type createParams struct {
	skuCode    string // explicit SkuCode, then the resolved SKU
	spec       string // value of the product's spec parameter (lower-cased)
	chargeType string // PREPAID | POSTPAID; "" until resolved (default POSTPAID)
	period     int64  // months, PREPAID only
	regionID   string
	zoneID     string
	token      string
	attrs      map[string]string
}

// createParams parses the shared purchase parameters.
func (c *call) createParams() (*createParams, error) {
	cp := &createParams{attrs: map[string]string{}}
	var err error
	if cp.skuCode, err = c.p.str("SkuCode"); err != nil {
		return nil, err
	}
	if cp.skuCode != "" && !skuCodePattern.MatchString(cp.skuCode) {
		return nil, invalidParam("SkuCode", "must be a SKU code such as euecs.s2.large.postpaid")
	}
	if c.ps.specParam != "" {
		spec, err := c.p.str(c.ps.specParam)
		if err != nil {
			return nil, err
		}
		spec = strings.ToLower(spec)
		switch {
		case spec != "" && !specPattern.MatchString(spec):
			return nil, invalidParam(c.ps.specParam, "is not a specification name")
		case spec != "" && cp.skuCode != "":
			return nil, invalidParam("SkuCode", "must not be combined with "+c.ps.specParam)
		}
		cp.spec = spec
	}
	ct, err := c.p.str("ChargeType", "InstanceChargeType")
	if err != nil {
		return nil, err
	}
	if cp.chargeType, err = parseChargeType(ct); err != nil {
		return nil, err
	}
	if cp.period, err = c.p.integer("Period", 1, 1, maxPeriodMonths); err != nil {
		return nil, err
	}
	unit, err := c.p.str("PeriodUnit")
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(unit) {
	case "", "month":
	case "year":
		if cp.period > maxPeriodMonths/12 {
			return nil, invalidParam("Period", fmt.Sprintf("must be between 1 and %d years", maxPeriodMonths/12))
		}
		cp.period *= 12
	default:
		return nil, invalidParam("PeriodUnit", "must be Month or Year")
	}
	if cp.regionID, err = c.p.str("RegionId"); err != nil {
		return nil, err
	}
	if cp.regionID != "" && !identifier.IsValidRegion(cp.regionID) {
		return nil, invalidParam("RegionId", "must be a region id such as cn-north-1")
	}
	if cp.zoneID, err = c.p.str("ZoneId"); err != nil {
		return nil, err
	}
	if cp.zoneID != "" {
		az, err := topology.ParseAZName(cp.zoneID)
		switch {
		case err != nil:
			return nil, invalidParam("ZoneId", "must be a zone id such as cn-north-1-a")
		case cp.regionID == "":
			cp.regionID = az.Region
		case az.Region != cp.regionID:
			return nil, invalidParam("ZoneId", "is not a zone of region "+cp.regionID)
		}
	}
	if cp.token, err = c.p.str("ClientToken"); err != nil {
		return nil, err
	}
	if cp.token != "" && (len(cp.token) > maxClientTokenLen || !clientTokenPattern.MatchString(cp.token)) {
		return nil, invalidParam("ClientToken", fmt.Sprintf("must be at most %d printable ASCII characters", maxClientTokenLen))
	}
	if c.ps.nameParam != "" {
		name, err := c.p.name(c.ps.nameParam)
		if err != nil {
			return nil, err
		}
		if name != "" {
			cp.attrs["name"] = name
		}
	}
	return cp, nil
}

// purchase buys and fulfils one resource.
func (s *server) purchase(c *call, cp *createParams) (*fulfilResult, *orderRecord, error) {
	if err := s.resolveSKU(c, cp); err != nil {
		return nil, nil, err
	}
	if err := s.place(c, cp); err != nil {
		return nil, nil, err
	}
	duration := int64(1)
	if cp.chargeType == chargePrepaid {
		duration = cp.period
	}
	o, err := s.createOrder(c, createOrderBody{
		Type: "NEW", ProductCode: c.product, SKUCode: cp.skuCode, RegionID: cp.regionID, ZoneID: cp.zoneID,
		ChargeType: cp.chargeType, Quantity: 1, Duration: duration, ClientToken: cp.token,
	})
	if err != nil {
		return nil, nil, err
	}
	// svc-order answers a known ClientToken with the order it already holds,
	// whatever the new request says: a token reused for a different purchase
	// must not silently return (and fulfil) the first one.
	if o.ProductCode != c.product || o.SKUCode != cp.skuCode || o.RegionID != cp.regionID ||
		(o.ZoneID != "" && o.ZoneID != cp.zoneID) || (o.ChargeType != "" && o.ChargeType != cp.chargeType) ||
		(cp.chargeType == chargePrepaid && o.Duration != 0 && o.Duration != duration) {
		return nil, o, errIdempotentMismatch
	}
	switch o.State {
	case orderCancelled:
		return nil, o, errOrderNotDeliverable
	case orderPendingPayment:
		if err := s.payOrder(c, o.OrderID); err != nil {
			return nil, o, err
		}
	}
	fr, err := s.fulfil(c, fulfilBody{
		OrderID: o.OrderID, OrderNo: o.OrderNo, ProductCode: c.product, Region: o.RegionID, ZoneID: o.ZoneID,
		SpecCode: o.SKUCode, ChargeType: cp.chargeType, Params: cp.attrs,
	})
	if err != nil {
		return nil, o, err
	}
	return fr, o, nil
}

// resolveSKU picks the SKU: an explicit SkuCode; else {product}.{spec}.{charge}
// from the product's spec parameter; else the product's only on-sale SKU for
// the charge type (euvpc, euoss, eueip, and eurds/eubs while each sells one
// spec). The charge type defaults to Postpaid.
func (s *server) resolveSKU(c *call, cp *createParams) error {
	all, err := s.skus(c)
	if err != nil {
		return err
	}
	find := func(code string) *catalogSKU {
		for i := range all {
			if all[i].SKUCode == code && all[i].ProductCode == c.product {
				return &all[i]
			}
		}
		return nil
	}
	other := func(ct string) string {
		if ct == chargePrepaid {
			return chargePostpaid
		}
		return chargePrepaid
	}
	var chosen *catalogSKU
	switch {
	case cp.skuCode != "":
		c.specParam = "SkuCode"
		if chosen = find(cp.skuCode); chosen == nil {
			return c.specNotFound()
		}
		if cp.chargeType != "" && chosen.ChargeType != cp.chargeType {
			return invalidParam("ChargeType", "does not match the charge type of SkuCode "+cp.skuCode)
		}
	case cp.spec != "":
		c.specParam = c.ps.specParam
		if cp.chargeType == "" {
			cp.chargeType = chargePostpaid
		}
		if chosen = find(skuFor(c.product, cp.spec, cp.chargeType)); chosen == nil {
			if find(skuFor(c.product, cp.spec, other(cp.chargeType))) != nil {
				return invalidParam("ChargeType", fmt.Sprintf("%s %s is not sold as %s",
					c.ps.specParam, cp.spec, apiChargeType(cp.chargeType)))
			}
			return c.specNotFound()
		}
	default:
		if cp.chargeType == "" {
			cp.chargeType = chargePostpaid
		}
		var candidates []*catalogSKU
		otherCharge := false
		for i := range all {
			sk := &all[i]
			if sk.ProductCode != c.product || sk.Status != skuStatusOnSale {
				continue
			}
			if sk.ChargeType == cp.chargeType {
				candidates = append(candidates, sk)
			} else {
				otherCharge = true
			}
		}
		switch {
		case len(candidates) == 1:
			chosen = candidates[0]
		case len(candidates) > 1:
			if c.ps.specParam != "" {
				return missingParam(c.ps.specParam)
			}
			return missingParam("SkuCode")
		case otherCharge:
			return invalidParam("ChargeType", c.product+" is not sold as "+apiChargeType(cp.chargeType))
		default:
			return errorsx.New("OperationDenied.NotOnSale", http.StatusConflict,
				fmt.Sprintf("%s is not on sale.", c.product))
		}
	}
	if chosen.Status != skuStatusOnSale {
		return c.specNotOnSale()
	}
	cp.skuCode = chosen.SKUCode
	cp.chargeType = chosen.ChargeType
	return nil
}

// place resolves the region (RegionId, the ZoneId's region, else the
// deployment's default region) against the catalogue's sellable inventory,
// and the zone of a zonal product (ZoneId, else the region's first zone).
// A regional product drops ZoneId, as svc-order does.
func (s *server) place(c *call, cp *createParams) error {
	if cp.regionID == "" {
		cp.regionID = s.defaultRegion
	}
	regions, err := s.regions(c)
	if err != nil {
		return err
	}
	var rg *catalogRegion
	for i := range regions {
		if regions[i].RegionID == cp.regionID {
			rg = &regions[i]
			break
		}
	}
	if rg == nil {
		return errorsx.New(codeInvalidRegion, http.StatusBadRequest,
			fmt.Sprintf("The specified RegionId %s does not exist or is not open for sale.", cp.regionID))
	}
	if !c.ps.zonal {
		cp.zoneID = ""
		return nil
	}
	if cp.zoneID == "" {
		if len(rg.Zones) == 0 {
			return contractError(s.catalog, "region "+rg.RegionID+" lists no zones")
		}
		cp.zoneID = rg.Zones[0].ZoneID
		return nil
	}
	for _, z := range rg.Zones {
		if z.ZoneID == cp.zoneID {
			return nil
		}
	}
	return errorsx.New(codeInvalidZone, http.StatusBadRequest,
		fmt.Sprintf("The specified ZoneId %s does not exist in region %s.", cp.zoneID, cp.regionID))
}
