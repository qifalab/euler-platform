package main

// The OpenAPI Action catalogue: what the svc-openapi facade
// (services/svc-openapi) serves behind {product}.api.euler.emoera.com, one
// entry per product Action with its parameters, response fields and the error
// codes the facade answers with. It is what the in-memory mode seeds (the
// Explorer dropdown, the action catalogue page and the docs read it), so it
// must stay in lockstep with svc-openapi's action table (resources.go,
// monitor.go), its parameter parsing (purchase.go, params.go) and its error
// vocabulary (errors.go). catalogue_test.go pins the Action list.
//
// Error codes are the facade's OpenAPI vocabulary ({Reason}.{Detail},
// e.g. InvalidInstanceId.NotFound), not product-prefixed platform codes.

import (
	"encoding/json"
	"fmt"
)

// catalogueVersion is the API version label the catalogue documents (the
// facade accepts any date version; the SDKs and the provider send this one).
const catalogueVersion = "2026-08-01"

// schemaNode is the JSON-Schema subset the catalogue uses for parameter and
// response descriptions. param_schema keeps its established shape
// ({"type":"object","required":[...],"properties":{...}}), which the
// devops-explorer frontend reads.
type schemaNode struct {
	Type        string                 `json:"type"`
	Description string                 `json:"description,omitempty"`
	Enum        []string               `json:"enum,omitempty"`
	Default     any                    `json:"default,omitempty"`
	Minimum     *int64                 `json:"minimum,omitempty"`
	Maximum     *int64                 `json:"maximum,omitempty"`
	MaxLength   int                    `json:"maxLength,omitempty"`
	MaxItems    int                    `json:"maxItems,omitempty"`
	Items       *schemaNode            `json:"items,omitempty"`
	Required    []string               `json:"required,omitempty"`
	Properties  map[string]*schemaNode `json:"properties,omitempty"`
}

// prop is one named parameter or response field.
type prop struct {
	name     string
	required bool
	node     *schemaNode
}

func pString(name, desc string) prop {
	return prop{name: name, node: &schemaNode{Type: "string", Description: desc}}
}

func pInt(name, desc string) prop {
	return prop{name: name, node: &schemaNode{Type: "integer", Description: desc}}
}

func pNumber(name, desc string) prop {
	return prop{name: name, node: &schemaNode{Type: "number", Description: desc}}
}

func pStrings(name, desc string) prop {
	return prop{name: name, node: &schemaNode{Type: "array", Description: desc, Items: &schemaNode{Type: "string"}}}
}

// pObjects is an array of objects (a Describe page, the datapoints of a metric).
func pObjects(name, desc string, fields ...prop) prop {
	item := object(fields...)
	return prop{name: name, node: &schemaNode{Type: "array", Description: desc, Items: item}}
}

func (p prop) req() prop { p.required = true; return p }

func (p prop) oneOf(values ...string) prop { p.node.Enum = values; return p }

func (p prop) def(v any) prop { p.node.Default = v; return p }

func (p prop) maxLen(n int) prop { p.node.MaxLength = n; return p }

func (p prop) maxItems(n int) prop { p.node.MaxItems = n; return p }

func (p prop) between(lo, hi int64) prop { p.node.Minimum, p.node.Maximum = &lo, &hi; return p }

// object assembles named props into an object schema.
func object(props ...prop) *schemaNode {
	n := &schemaNode{Type: "object", Properties: make(map[string]*schemaNode, len(props))}
	for _, p := range props {
		if _, dup := n.Properties[p.name]; dup {
			panic("catalogue: duplicate property " + p.name)
		}
		n.Properties[p.name] = p.node
		if p.required {
			n.Required = append(n.Required, p.name)
		}
	}
	return n
}

// --- error vocabulary (svc-openapi errors.go) ---------------------------------

// commonErrorCodes are answered by the facade's dispatch for any Action of a
// product: a missing/malformed parameter (including Action and Version), an
// Action name the product does not serve, and the generic failures (a
// downstream outage or timeout is ServiceUnavailable, which the SDKs retry).
var commonErrorCodes = []string{
	"MissingParameter",
	"InvalidParameter",
	"InvalidAction.NotFound",
	"ServiceUnavailable",
	"InternalError",
}

// purchaseErrorCodes are the purchase flow's verdicts shared by every create
// (catalogue → order → payment → fulfilment).
var purchaseErrorCodes = []string{
	"InvalidRegionId.NotFound",
	"InvalidSkuCode.NotFound",
	"InvalidSkuCode.NotOnSale",
	"OperationDenied.NotOnSale",
	"QuotaExceeded",
	"InsufficientBalance",
	"IdempotentParameterMismatch",
	"IncorrectOrderStatus",
	"ProvisionFailed",
}

// --- products -----------------------------------------------------------------

// catalogueProduct mirrors svc-openapi's productSpec for one resource product.
type catalogueProduct struct {
	code     string
	resource string // what one resource is called in descriptions
	// idParam names one resource, idsParam a list ("" = none), listKey the
	// Describe array, nameParam the display-name parameter ("" = none).
	idParam, idsParam, listKey, nameParam string
	// specParam selects the SKU's spec part ("" = SKU follows from ChargeType).
	specParam, specExample string
	skuExample             string // a SKU code the catalogue sells
	zonal                  bool   // placed in one availability zone
	billed                 bool   // Describe carries ChargeType/SkuCode/ExpiredTime
	runningStatus          string
	notFound, badStatus    string
	// invalidAttr is the code of a parameter the orchestrator refuses
	// ("" = InvalidParameter, already common).
	invalidAttr string
	// fields are the product's own Describe fields (after the shared ones).
	fields []prop
}

var (
	productECS = &catalogueProduct{
		code: "euecs", resource: "instance", idParam: "InstanceId", idsParam: "InstanceIds", listKey: "Instances",
		nameParam: "InstanceName", specParam: "InstanceType", specExample: "s2.large", skuExample: "euecs.s2.large.postpaid", zonal: true, billed: true,
		runningStatus: "Running", notFound: "InvalidInstanceId.NotFound", badStatus: "IncorrectInstanceStatus",
		fields: []prop{pString("InstanceType", "Instance type (the spec part of SkuCode), e.g. s2.large.")},
	}
	productRDS = &catalogueProduct{
		code: "eurds", resource: "database instance", idParam: "InstanceId", idsParam: "InstanceIds", listKey: "Instances",
		nameParam: "InstanceName", specParam: "InstanceClass", specExample: "mysql8.small", skuExample: "eurds.mysql8.small.postpaid", zonal: true, billed: true,
		runningStatus: "Running", notFound: "InvalidInstanceId.NotFound", badStatus: "IncorrectInstanceStatus",
		fields: []prop{
			pString("InstanceClass", "Instance class (the spec part of SkuCode)."),
			pString("Engine", "Database engine.").oneOf("MySQL"),
			pString("EngineVersion", "Engine version, e.g. 8.0."),
		},
	}
	productVPC = &catalogueProduct{
		code: "euvpc", resource: "VPC", idParam: "VpcId", idsParam: "VpcIds", listKey: "Vpcs",
		nameParam: "VpcName", skuExample: "euvpc.standard.postpaid", runningStatus: "Available",
		notFound: "InvalidVpcId.NotFound", badStatus: "IncorrectVpcStatus", invalidAttr: "InvalidCidrBlock",
		fields: []prop{pString("CidrBlock", "IPv4 CIDR block of the VPC.")},
	}
	productBS = &catalogueProduct{
		code: "eubs", resource: "disk", idParam: "DiskId", idsParam: "DiskIds", listKey: "Disks",
		nameParam: "DiskName", specParam: "DiskCategory", specExample: "essd", skuExample: "eubs.essd.postpaid", zonal: true, billed: true,
		runningStatus: "Available", notFound: "InvalidDiskId.NotFound", badStatus: "IncorrectDiskStatus",
		fields: []prop{
			pString("DiskCategory", "Disk category (the spec part of SkuCode)."),
			pInt("Size", "Disk size in GiB."),
		},
	}
	productEIP = &catalogueProduct{
		code: "eueip", resource: "EIP", idParam: "AllocationId", idsParam: "AllocationIds", listKey: "EipAddresses",
		nameParam: "Name", skuExample: "eueip.traffic.postpaid", zonal: true, billed: true, runningStatus: "Available",
		notFound: "InvalidAllocationId.NotFound", badStatus: "IncorrectEipStatus",
		fields: []prop{
			pInt("Bandwidth", "Peak bandwidth in Mbps."),
			pString("InternetChargeType", "How traffic is billed.").oneOf("PayByBandwidth", "PayByTraffic"),
		},
	}
	productOSS = &catalogueProduct{
		code: "euoss", resource: "bucket", idParam: "Bucket", listKey: "Buckets", skuExample: "euoss.standard.postpaid", runningStatus: "Available",
		notFound: "InvalidBucketName.NotFound", badStatus: "IncorrectBucketStatus", invalidAttr: "InvalidBucketName",
	}
)

// statuses is the lifecycle vocabulary of Describe (svc-openapi productSpec.status).
func (p *catalogueProduct) statuses() []string {
	return []string{"Creating", p.runningStatus, "Stopped", "Modifying", "Locked", "Preempting",
		"Expired", "Deleting", "Deleted", "CreateFailed", "Unknown"}
}

func (p *catalogueProduct) statusField(desc string) prop {
	return pString("Status", desc).oneOf(p.statuses()...)
}

// item is one resource as Describe renders it (baseView + billedView + fields).
func (p *catalogueProduct) item() []prop {
	out := []prop{
		pString(p.idParam, "The "+p.resource+" id."),
		pString("RegionId", "Region of the "+p.resource+"."),
		p.statusField("Lifecycle status."),
		pString("CreationTime", "Creation time (RFC 3339)."),
	}
	if p.nameParam != "" {
		out = append(out, pString(p.nameParam, "Display name (empty when none was set)."))
	}
	if p.zonal {
		out = append(out, pString("ZoneId", "Availability zone of the "+p.resource+"."))
	}
	if p.billed {
		out = append(out,
			pString("ChargeType", "Billing method.").oneOf("Prepaid", "Postpaid"),
			pString("SkuCode", "Full SKU code, e.g. "+p.skuExample+"."),
			pString("ExpiredTime", "Expiry time of a Prepaid "+p.resource+" (RFC 3339); empty for Postpaid."),
		)
	}
	return append(out, p.fields...)
}

func (p *catalogueProduct) idProp() prop {
	return pString(p.idParam, "The "+p.resource+" id.").req()
}

func (p *catalogueProduct) nameProp(required bool) prop {
	n := pString(p.nameParam, "Display name: at most 64 printable characters.").maxLen(64)
	if required {
		n = n.req()
	}
	return n
}

// --- parameter sets -----------------------------------------------------------

func pageParams() []prop {
	return []prop{
		pInt("PageNumber", "Page to return, from 1.").def(1).between(1, 100000),
		pInt("PageSize", "Entries per page.").def(10).between(1, 100),
	}
}

func pageFields() []prop {
	return []prop{
		pInt("TotalCount", "Number of matching entries over all pages."),
		pInt("PageNumber", "The page returned."),
		pInt("PageSize", "The page size applied."),
	}
}

// purchaseParams are the parameters every create shares (svc-openapi
// createParams), plus the product's spec and name parameters.
func (p *catalogueProduct) purchaseParams() []prop {
	var out []prop
	if p.specParam != "" {
		out = append(out, pString(p.specParam, fmt.Sprintf(
			"Specification, e.g. %s; with ChargeType it selects the SKU %s.{%s}.{charge}. Required unless SkuCode is given or the product sells a single specification for the charge type.",
			p.specExample, p.code, p.specParam)))
	}
	skuDesc := "Full SKU code, e.g. " + p.skuExample + ". Optional: by default the product's only on-sale SKU for the charge type is used."
	if p.specParam != "" {
		skuDesc = "Full SKU code, e.g. " + p.skuExample + "; an alternative to " + p.specParam + " (not combinable with it)."
	}
	out = append(out,
		pString("SkuCode", skuDesc).maxLen(64),
		pString("ChargeType", "Billing method (alias InstanceChargeType; PrePaid/PREPAID spellings accepted).").
			oneOf("Prepaid", "Postpaid").def("Postpaid"),
		pInt("Period", "Prepaid term in PeriodUnit units (at most 60 months / 5 years); ignored for Postpaid.").
			def(1).between(1, 60),
		pString("PeriodUnit", "Unit of Period.").oneOf("Month", "Year").def("Month"),
	)
	if p.zonal {
		out = append(out,
			pString("RegionId", "Region to create in, e.g. cn-north-1. Defaults to the region of ZoneId, else the deployment's default region."),
			pString("ZoneId", "Availability zone, e.g. cn-north-1-a; must lie in RegionId. Defaults to the region's first zone."),
		)
	} else {
		out = append(out, pString("RegionId", "Region to create in, e.g. cn-north-1. Defaults to the deployment's default region."))
	}
	out = append(out, pString("ClientToken",
		"Idempotency token (at most 64 printable ASCII characters, no spaces). A retry with the same token and parameters returns the same order and resource; the same token with different parameters fails with IdempotentParameterMismatch.").
		maxLen(64))
	if p.nameParam != "" {
		out = append(out, p.nameProp(false))
	}
	return out
}

func (p *catalogueProduct) createErrors(extra ...string) []string {
	codes := append([]string{}, purchaseErrorCodes...)
	if p.zonal {
		codes = append(codes, "InvalidZoneId.NotFound")
	}
	if p.specParam != "" {
		codes = append(codes, "Invalid"+p.specParam+".NotFound", "Invalid"+p.specParam+".NotOnSale")
	}
	if p.invalidAttr != "" {
		codes = append(codes, p.invalidAttr)
	}
	return append(codes, extra...)
}

func (p *catalogueProduct) createResponse(extra ...prop) []prop {
	out := []prop{
		pString(p.idParam, "Id of the new "+p.resource+"."),
		pString("OrderId", "The purchase order (svc-order)."),
		p.statusField("Usually Creating: poll the Describe action until it reads " + p.runningStatus + "."),
	}
	return append(out, extra...)
}

// --- Action builders ----------------------------------------------------------

// entry is one catalogue Action before it is rendered into an Action.
type entry struct {
	product, action, desc string
	params, response      []prop
	errors                []string
}

func (p *catalogueProduct) create(action, desc string, own []prop, extraErrors ...string) entry {
	return entry{product: p.code, action: action, desc: desc,
		params: append(own, p.purchaseParams()...), response: p.createResponse(), errors: p.createErrors(extraErrors...)}
}

func (p *catalogueProduct) describe(action string) entry {
	params := []prop{pString(p.idParam, "Return only this "+p.resource+".")}
	params = append(params,
		pStrings(p.idsParam, "Return only these "+p.resource+"s: a JSON array, repeated key, comma-separated list or "+p.idsParam+".N keys.").maxItems(100),
		pString("RegionId", "Return only "+p.resource+"s of this region."),
	)
	params = append(params, pageParams()...)
	response := append([]prop{pObjects(p.listKey, "The "+p.resource+"s of the page, newest first.", p.item()...)}, pageFields()...)
	return entry{product: p.code, action: action,
		desc: fmt.Sprintf("Lists the caller's %ss (released ones excluded). An id that does not exist yields an empty %s, not an error.",
			p.resource, p.listKey),
		params: params, response: response, errors: nil}
}

func (p *catalogueProduct) power(action, verb string) entry {
	return entry{product: p.code, action: action, desc: verb + " one " + p.resource + ".",
		params:   []prop{p.idProp()},
		response: []prop{pString(p.idParam, "The "+p.resource+" id."), p.statusField("Status after the operation.")},
		errors:   []string{p.notFound, p.badStatus}}
}

func (p *catalogueProduct) modifySpec() entry {
	return entry{product: p.code, action: "ModifyInstanceSpec",
		desc: "Changes the specification of a Postpaid " + p.resource + " within its charge type. A Prepaid " + p.resource + " needs an upgrade order (OperationDenied.UpgradeRequiresOrder).",
		params: []prop{
			p.idProp(),
			pString(p.specParam, "Target specification, e.g. "+p.specExample+". Required unless SkuCode is given."),
			pString("SkuCode", "Target SKU code with the resource's charge type; an alternative to "+p.specParam+".").maxLen(64),
		},
		response: []prop{
			pString(p.idParam, "The "+p.resource+" id."),
			pString("SkuCode", "The SKU now in effect."),
			p.statusField("Status after the change."),
		},
		errors: []string{p.notFound, p.badStatus, "Invalid" + p.specParam + ".NotFound", "Invalid" + p.specParam + ".NotOnSale",
			"InvalidSkuCode.NotFound", "InvalidSkuCode.NotOnSale", "OperationDenied.UpgradeRequiresOrder"}}
}

func (p *catalogueProduct) rename(action string) entry {
	codes := []string{p.notFound, p.badStatus}
	if p.invalidAttr != "" {
		codes = append(codes, p.invalidAttr)
	}
	return entry{product: p.code, action: action, desc: "Renames one " + p.resource + ".",
		params:   []prop{p.idProp(), p.nameProp(true)},
		response: []prop{pString(p.idParam, "The "+p.resource+" id."), pString(p.nameParam, "The new display name.")},
		errors:   codes}
}

func (p *catalogueProduct) release(action string) entry {
	return entry{product: p.code, action: action,
		desc: "Releases one or more " + p.resource + "s (" + p.idParam + " and/or " + p.idsParam + "; at least one id). Every id is checked to be the caller's before any is released.",
		params: []prop{
			pString(p.idParam, "The "+p.resource+" to release (required unless "+p.idsParam+" is given)."),
			pStrings(p.idsParam, "The "+p.resource+"s to release (required unless "+p.idParam+" is given).").maxItems(100),
		},
		response: []prop{
			pString(p.idParam, "The released "+p.resource+" (when exactly one was named)."),
			pStrings(p.idsParam, "Every released "+p.resource+"."),
		},
		errors: []string{p.notFound, p.badStatus}}
}

// --- the catalogue ------------------------------------------------------------

func catalogueEntries() []entry {
	ecs, rds, vpc, bs, eip, oss := productECS, productRDS, productVPC, productBS, productEIP, productOSS

	bucketParam := pString("Bucket", "Bucket name (alias BucketName): 3-63 lowercase letters, digits, dots and hyphens, starting and ending with a letter or digit.").req()
	alarmRule := []prop{
		pString("RuleId", "Rule id."),
		pString("ProductCode", "Product the rule watches, e.g. euecs."),
		pString("ResourceType", "Resource type within the product, e.g. instance."),
		pString("MetricName", "Metric, e.g. cpu_utilization."),
		pString("Threshold", "Threshold (decimal string, at most 4 decimal places)."),
		pString("ComparisonOperator", "Comparison against Threshold.").oneOf(comparisonOperators...),
		pInt("Period", "Evaluation period in seconds."),
		pInt("EvaluationCount", "Consecutive periods that must breach before alarming."),
		pStrings("NotificationChannels", "Notification channels."),
		pString("Status", "Rule status.").oneOf("Enabled", "Disabled"),
		pInt("RuleVersion", "Optimistic-lock version of the rule."),
		pString("CreationTime", "Creation time."),
		pString("UpdateTime", "Last update time."),
	}
	ruleSettings := []prop{
		pString("Threshold", "Threshold as a decimal number (at most 4 decimal places)."),
		pString("ComparisonOperator", "Comparison against Threshold (>, >=, <, <= and 1-4 are accepted too).").oneOf(comparisonOperators...),
		pInt("Period", "Evaluation period in seconds.").between(0, 86400),
		pInt("EvaluationCount", "Consecutive breaching periods before alarming.").between(0, 1440),
		pStrings("NotificationChannels", "Notification channels (each 1-128 of [A-Za-z0-9_.:@-]).").maxItems(10),
	}

	return []entry{
		// euecs ----------------------------------------------------------------
		ecs.create("RunInstances", "Buys and creates one ECS instance (catalogue → order → payment → fulfilment).",
			[]prop{pString("ImageId", "Image id (accepted and shape-checked; the instance's image is not recorded yet).")}),
		ecs.describe("DescribeInstances"),
		ecs.power("StartInstance", "Starts"),
		ecs.power("StopInstance", "Stops"),
		ecs.power("RebootInstance", "Reboots"),
		ecs.modifySpec(),
		ecs.rename("ModifyInstanceAttribute"),
		ecs.release("TerminateInstances"),

		// eurds ----------------------------------------------------------------
		rds.create("CreateInstance", "Buys and creates one MySQL instance.", []prop{
			pString("Engine", "Database engine.").oneOf("MySQL").def("MySQL"),
			pString("EngineVersion", "Engine version, e.g. 5.7 or 8.0 (validated by the orchestrator)."),
		}),
		rds.describe("DescribeInstances"),
		rds.power("StartInstance", "Starts"),
		rds.power("StopInstance", "Stops"),
		rds.power("RestartInstance", "Restarts"),
		rds.modifySpec(),
		rds.rename("ModifyInstanceAttribute"),
		rds.release("DeleteInstance"),

		// euvpc ----------------------------------------------------------------
		vpc.create("CreateVpc", "Creates one VPC.", []prop{
			pString("CidrBlock", "IPv4 CIDR block, e.g. 192.168.0.0/16; must not overlap another VPC of the account (InvalidCidrBlock).").req(),
		}),
		vpc.describe("DescribeVpcs"),
		vpc.rename("ModifyVpcAttribute"),
		vpc.release("DeleteVpc"),

		// eubs -----------------------------------------------------------------
		bs.create("CreateDisk", "Buys and creates one block-storage disk.", []prop{
			pInt("Size", "Disk size in GiB (range checked by the orchestrator).").req().between(1, 1<<20),
		}),
		bs.describe("DescribeDisks"),
		bs.rename("ModifyDiskAttribute"),
		bs.release("DeleteDisk"),

		// eueip ----------------------------------------------------------------
		eip.create("AllocateEipAddress", "Buys and allocates one elastic IP.", []prop{
			pInt("Bandwidth", "Peak bandwidth in Mbps.").between(1, 1<<20),
		}),
		eip.describe("DescribeEipAddresses"),
		eip.release("ReleaseEipAddress"),

		// euoss (control plane; a bucket is addressed by its name) --------------
		{product: oss.code, action: "CreateBucket", desc: "Creates one bucket.",
			params: append([]prop{bucketParam, pString("StorageClass", "Storage class (only Standard is on sale).").oneOf("Standard").def("Standard")},
				oss.purchaseParams()...),
			response: oss.createResponse(
				pString("BucketName", "The bucket name (same as Bucket)."),
				pString("ResourceId", "The bucket's resource id."),
			),
			errors: oss.createErrors("BucketAlreadyExists")},
		{product: oss.code, action: "ListBuckets", desc: "Lists the caller's buckets by name.",
			params: append([]prop{pString("Prefix", "Return only buckets whose name starts with this prefix.")}, pageParams()...),
			response: append([]prop{pObjects("Buckets", "The buckets of the page, by name.",
				pString("Bucket", "Bucket name."),
				pString("RegionId", "Region of the bucket."),
				pString("StorageClass", "Storage class.").oneOf("Standard"),
				oss.statusField("Lifecycle status."),
				pString("CreationTime", "Creation time (RFC 3339)."),
			)}, pageFields()...)},
		{product: oss.code, action: "DeleteBucket", desc: "Deletes the caller's bucket of that name.",
			params:   []prop{bucketParam},
			response: []prop{pString("Bucket", "The deleted bucket.")},
			errors:   []string{"InvalidBucketName", oss.notFound, oss.badStatus}},

		// eumon ----------------------------------------------------------------
		{product: "eumon", action: "DescribeAlarmRules", desc: "Lists the caller's alarm rules.",
			params: append([]prop{
				pString("RuleId", "Return only this rule."),
				pStrings("RuleIds", "Return only these rules.").maxItems(100),
				pString("ProductCode", "Return only rules of this product (alias Namespace)."),
				pString("MetricName", "Return only rules on this metric."),
			}, pageParams()...),
			response: append([]prop{pObjects("AlarmRules", "The rules of the page, by RuleId.", alarmRule...)}, pageFields()...)},
		{product: "eumon", action: "CreateAlarmRule",
			desc: "Creates an alarm rule. Rules are keyed on (ProductCode, ResourceType, MetricName) per account: creating an existing one updates it.",
			params: append([]prop{
				pString("ProductCode", "Product to watch, e.g. euecs (alias Namespace).").req(),
				pString("ResourceType", "Resource type within the product, e.g. instance."),
				pString("MetricName", "Metric, e.g. cpu_utilization.").req(),
				pString("Threshold", "Threshold as a decimal number (at most 4 decimal places).").req(),
			}, ruleSettings[1:]...),
			response: []prop{pString("RuleId", "Id of the rule."), pInt("RuleVersion", "Version of the rule.")}},
		{product: "eumon", action: "ModifyAlarmRule",
			desc: "Changes an alarm rule; at least one setting (or Status) is required.",
			params: append(append([]prop{pString("RuleId", "The rule to change.").req()}, ruleSettings...),
				pString("Status", "Enable or disable the rule.").oneOf("Enabled", "Disabled"),
				pInt("RuleVersion", "Optimistic lock: the version read; omitted, the current version is used."),
			),
			response: []prop{pString("RuleId", "Id of the rule."), pInt("RuleVersion", "New version of the rule.")},
			errors:   []string{"InvalidRuleId.NotFound", "ConcurrentModification"}},
		{product: "eumon", action: "DeleteAlarmRule", desc: "Deletes an alarm rule.",
			params:   []prop{pString("RuleId", "The rule to delete.").req()},
			response: []prop{pString("RuleId", "Id of the deleted rule.")},
			errors:   []string{"InvalidRuleId.NotFound"}},
		{product: "eumon", action: "DescribeMetricData", desc: "Returns a metric series of one of the caller's resources.",
			params: []prop{
				pString("MetricName", "Metric to read.").req().oneOf(metricNames...),
				pString("ResourceId", "The caller's resource (any product) whose series to read (alias InstanceId).").req(),
				pString("StartTime", "Start of the range (RFC 3339, Unix seconds or Unix milliseconds); defaults to one hour before EndTime."),
				pString("EndTime", "End of the range (RFC 3339, Unix seconds or Unix milliseconds); defaults to now. The range spans at most 31 days."),
				pInt("Period", "Step in seconds; the range must not yield more than 1440 points.").def(60).between(10, 86400),
			},
			response: []prop{
				pString("MetricName", "The metric."),
				pString("ResourceId", "The resource."),
				pInt("Period", "Step in seconds."),
				pObjects("Datapoints", "The series, oldest first.",
					pString("Timestamp", "Point time (RFC 3339, UTC)."), pNumber("Value", "Point value.")),
				pString("Source", "Where the series comes from: prometheus, or synthetic (a stand-in series served outside production only).").oneOf("prometheus", "synthetic"),
			},
			errors: []string{"InvalidResourceId.NotFound"}},
	}
}

// metricNames are the metrics svc-monitor serves (services/svc-monitor
// metrics.go metricQueries).
var metricNames = []string{"cpu_utilization", "memory_utilization", "request_count"}

// comparisonOperators is the ComparisonOperator vocabulary (svc-openapi monitor.go).
var comparisonOperators = []string{
	"GreaterThanThreshold", "GreaterThanOrEqualToThreshold", "LessThanThreshold", "LessThanOrEqualToThreshold",
}

// facadeCatalogue renders the catalogue into Actions: param_schema is an object
// schema (with the Action's description), response_schema the Data object,
// error_codes the dispatch codes plus the Action's own (deduplicated, in order).
func facadeCatalogue() []Action {
	entries := catalogueEntries()
	out := make([]Action, 0, len(entries))
	for _, e := range entries {
		params := object(e.params...)
		params.Description = e.desc
		ps, err := json.Marshal(params)
		if err != nil {
			panic(fmt.Sprintf("catalogue %s.%s params: %v", e.product, e.action, err))
		}
		rs, err := json.Marshal(object(e.response...))
		if err != nil {
			panic(fmt.Sprintf("catalogue %s.%s response: %v", e.product, e.action, err))
		}
		out = append(out, Action{
			ID: actionID(e.product, e.action), ProductCode: e.product, ActionName: e.action,
			Version: catalogueVersion, ParamSchema: ps, ResponseSchema: rs,
			ErrorCodes: dedupe(append(append([]string{}, commonErrorCodes...), e.errors...)),
		})
	}
	return out
}

func dedupe(codes []string) []string {
	seen := make(map[string]bool, len(codes))
	out := codes[:0]
	for _, c := range codes {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
