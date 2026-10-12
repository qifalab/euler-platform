package main

import (
	"strconv"
	"strings"
)

// productSpec describes how one product's resources appear in the API: the
// parameter vocabulary, the error codes and the Describe view.
type productSpec struct {
	code string
	// idParam names one resource (InstanceId), idsParam a list of them
	// (InstanceIds, "" when the product has no plural form) and listKey the
	// Describe response's array (Instances).
	idParam, idsParam, listKey string
	// nameParam is the display-name parameter, stored as the resource
	// attribute "name" ("" when the product has none).
	nameParam string
	// specParam selects the SKU's spec part ({product}.{spec}.{charge},
	// svc-catalog naming); "" when the SKU follows from the charge type.
	specParam string
	// zonal products are placed in one availability zone (svc-catalog
	// regionScope ZONAL); creates without ZoneId take the region's first zone.
	zonal bool
	// runningStatus is how the RUNNING lifecycle state reads in this API.
	runningStatus string
	// notFound / badStatus are the 404 and 409 of a resource-addressed action.
	notFound, badStatus string
	// invalidAttr is the code of a parameter the orchestrator refuses
	// (Resource.InvalidAttribute); "" = InvalidParameter.
	invalidAttr string
	// view renders one resource for Describe.
	view func(ps *productSpec, r *resourceRecord) map[string]any
}

// products is the phase-1 product set served by the facade.
var products = map[string]*productSpec{
	"euecs": {
		code: "euecs", idParam: "InstanceId", idsParam: "InstanceIds", listKey: "Instances",
		nameParam: "InstanceName", specParam: "InstanceType", zonal: true, runningStatus: "Running",
		notFound: "InvalidInstanceId.NotFound", badStatus: "IncorrectInstanceStatus",
		view: func(ps *productSpec, r *resourceRecord) map[string]any {
			m := billedView(ps, r)
			m["InstanceType"] = specOf(ps.code, r.SpecCode)
			return m
		},
	},
	"eurds": {
		code: "eurds", idParam: "InstanceId", idsParam: "InstanceIds", listKey: "Instances",
		nameParam: "InstanceName", specParam: "InstanceClass", zonal: true, runningStatus: "Running",
		notFound: "InvalidInstanceId.NotFound", badStatus: "IncorrectInstanceStatus",
		view: func(ps *productSpec, r *resourceRecord) map[string]any {
			m := billedView(ps, r)
			m["InstanceClass"] = specOf(ps.code, r.SpecCode)
			m["Engine"] = "MySQL"
			m["EngineVersion"] = r.Attributes["engineVersion"]
			return m
		},
	},
	"euvpc": {
		code: "euvpc", idParam: "VpcId", idsParam: "VpcIds", listKey: "Vpcs",
		nameParam: "VpcName", runningStatus: "Available",
		notFound: "InvalidVpcId.NotFound", badStatus: "IncorrectVpcStatus", invalidAttr: "InvalidCidrBlock",
		view: func(ps *productSpec, r *resourceRecord) map[string]any {
			m := baseView(ps, r)
			m["CidrBlock"] = r.Attributes["cidrBlock"]
			return m
		},
	},
	"eubs": {
		code: "eubs", idParam: "DiskId", idsParam: "DiskIds", listKey: "Disks",
		nameParam: "DiskName", specParam: "DiskCategory", zonal: true, runningStatus: "Available",
		notFound: "InvalidDiskId.NotFound", badStatus: "IncorrectDiskStatus",
		view: func(ps *productSpec, r *resourceRecord) map[string]any {
			m := billedView(ps, r)
			m["DiskCategory"] = specOf(ps.code, r.SpecCode)
			m["Size"] = intAttr(r.Attributes["sizeGb"])
			return m
		},
	},
	"eueip": {
		code: "eueip", idParam: "AllocationId", idsParam: "AllocationIds", listKey: "EipAddresses",
		nameParam: "Name", zonal: true, runningStatus: "Available",
		notFound: "InvalidAllocationId.NotFound", badStatus: "IncorrectEipStatus",
		view: func(ps *productSpec, r *resourceRecord) map[string]any {
			m := billedView(ps, r)
			m["Bandwidth"] = intAttr(r.Attributes["bandwidthMbps"])
			// eueip.bandwidth.prepaid / eueip.traffic.postpaid
			if specOf(ps.code, r.SpecCode) == "traffic" {
				m["InternetChargeType"] = "PayByTraffic"
			} else {
				m["InternetChargeType"] = "PayByBandwidth"
			}
			return m
		},
	},
	// euoss control plane: a bucket is addressed by its name (the resource
	// attribute "bucket"), not by its resource id.
	"euoss": {
		code: "euoss", idParam: "Bucket", listKey: "Buckets", runningStatus: "Available",
		notFound: "InvalidBucketName.NotFound", badStatus: "IncorrectBucketStatus", invalidAttr: "InvalidBucketName",
		view: func(ps *productSpec, r *resourceRecord) map[string]any {
			return map[string]any{
				"Bucket":       r.Attributes["bucket"],
				"RegionId":     r.Region,
				"StorageClass": titleCase(specOf(ps.code, r.SpecCode)),
				"Status":       ps.status(r.State),
				"CreationTime": r.CreatedAt,
			}
		},
	},
	"eumon": {
		code: "eumon", idParam: "RuleId", idsParam: "RuleIds", listKey: "AlarmRules",
		notFound: "InvalidRuleId.NotFound", badStatus: "IncorrectRuleStatus",
	},
}

// baseView is the record every resource product shares.
func baseView(ps *productSpec, r *resourceRecord) map[string]any {
	m := map[string]any{
		ps.idParam:     r.ResourceID,
		"RegionId":     r.Region,
		"Status":       ps.status(r.State),
		"CreationTime": r.CreatedAt,
	}
	if ps.nameParam != "" {
		m[ps.nameParam] = r.Attributes["name"]
	}
	if ps.zonal {
		m["ZoneId"] = r.Zone
	}
	return m
}

// billedView adds the purchase fields of a product sold per instance.
func billedView(ps *productSpec, r *resourceRecord) map[string]any {
	m := baseView(ps, r)
	m["ChargeType"] = apiChargeType(r.ChargeType)
	m["SkuCode"] = r.SpecCode
	m["ExpiredTime"] = r.ExpiredAt
	return m
}

// status renders a lifecycle state (pkg-go/resource) in the API vocabulary.
func (ps *productSpec) status(state string) string {
	switch state {
	case "INIT", stateCreating:
		return "Creating"
	case "RUNNING":
		return ps.runningStatus
	case "STOPPED":
		return "Stopped"
	case "UPGRADING":
		return "Modifying"
	case "LOCKED":
		return "Locked"
	case "PREEMPTING":
		return "Preempting"
	case "EXPIRED":
		return "Expired"
	case "RELEASING":
		return "Deleting"
	case stateReleased:
		return "Deleted"
	case "CREATE_FAILED":
		return "CreateFailed"
	}
	return "Unknown"
}

// specOf is the spec part of a SKU code: euecs.s2.large.postpaid → s2.large.
// The orchestrator's demo seeds carry codes without the charge suffix.
func specOf(product, sku string) string {
	s := strings.TrimPrefix(sku, product+".")
	s = strings.TrimSuffix(s, ".prepaid")
	return strings.TrimSuffix(s, ".postpaid")
}

// skuFor is the SKU code of a spec sold under a charge type.
func skuFor(product, spec, chargeType string) string {
	return product + "." + spec + "." + strings.ToLower(chargeType)
}

// apiChargeType renders a charge type the way svc-api-meta documents the
// ChargeType parameter (Prepaid | Postpaid).
func apiChargeType(ct string) string {
	switch ct {
	case chargePrepaid:
		return "Prepaid"
	case chargePostpaid:
		return "Postpaid"
	}
	return ct
}

// parseChargeType accepts PrePaid/Prepaid/PREPAID/Pre_Paid (and Post*); ""
// stays "" (the caller applies the default).
func parseChargeType(v string) (string, error) {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), "_", "")) {
	case "":
		return "", nil
	case chargePrepaid:
		return chargePrepaid, nil
	case chargePostpaid:
		return chargePostpaid, nil
	}
	return "", invalidParam("ChargeType", "must be Prepaid or Postpaid")
}

// intAttr renders a numeric attribute as a number (nil when unset).
func intAttr(v string) any {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	return nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
