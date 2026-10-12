package main

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	errorsx "github.com/qifalab/euler-platform/errors"
)

const (
	defaultPageSize = 10
	maxPageSize     = 100
	maxPageNumber   = 100000
)

// actionTable is the Action vocabulary per product. The names and parameters
// follow svc-api-meta's action catalogue and what sdk/terraform calls.
func (s *server) actionTable() map[string]map[string]actionFunc {
	return map[string]map[string]actionFunc{
		"euecs": {
			"RunInstances":            s.createAction(ecsCreateParams, nil),
			"DescribeInstances":       s.describe,
			"StartInstance":           s.power("start"),
			"StopInstance":            s.power("stop"),
			"RebootInstance":          s.power("restart"),
			"ModifyInstanceSpec":      s.modifySpec,
			"ModifyInstanceAttribute": s.rename,
			"TerminateInstances":      s.release,
		},
		"eurds": {
			"CreateInstance":          s.createAction(rdsCreateParams, nil),
			"DescribeInstances":       s.describe,
			"StartInstance":           s.power("start"),
			"StopInstance":            s.power("stop"),
			"RestartInstance":         s.power("restart"),
			"ModifyInstanceSpec":      s.modifySpec,
			"ModifyInstanceAttribute": s.rename,
			"DeleteInstance":          s.release,
		},
		"euvpc": {
			"CreateVpc":          s.createAction(vpcCreateParams, nil),
			"DescribeVpcs":       s.describe,
			"ModifyVpcAttribute": s.rename,
			"DeleteVpc":          s.release,
		},
		"eubs": {
			"CreateDisk":          s.createAction(diskCreateParams, nil),
			"DescribeDisks":       s.describe,
			"ModifyDiskAttribute": s.rename,
			"DeleteDisk":          s.release,
		},
		"eueip": {
			"AllocateEipAddress":   s.createAction(eipCreateParams, nil),
			"DescribeEipAddresses": s.describe,
			"ReleaseEipAddress":    s.release,
		},
		"euoss": {
			"CreateBucket": s.createAction(bucketCreateParams, bucketCreated),
			"ListBuckets":  s.listBuckets,
			"DeleteBucket": s.deleteBucket,
		},
		"eumon": {
			"DescribeAlarmRules": s.describeAlarmRules,
			"CreateAlarmRule":    s.createAlarmRule,
			"ModifyAlarmRule":    s.modifyAlarmRule,
			"DeleteAlarmRule":    s.deleteAlarmRuleAction,
			"DescribeMetricData": s.describeMetricData,
		},
	}
}

// --- create ------------------------------------------------------------------

// createAction runs a purchase. extra parses the product's own creation
// parameters into cp.attrs; created (optional) adjusts the response.
func (s *server) createAction(extra func(c *call, cp *createParams) error,
	created func(m map[string]any, cp *createParams, fr *fulfilResult)) actionFunc {
	return func(c *call) (any, error) {
		cp, err := c.createParams()
		if err != nil {
			return nil, err
		}
		if extra != nil {
			if err := extra(c, cp); err != nil {
				return nil, err
			}
		}
		fr, o, err := s.purchase(c, cp)
		if err != nil {
			return nil, err
		}
		m := map[string]any{
			c.ps.idParam: fr.ResourceID,
			"OrderId":    strconv.FormatInt(o.OrderID, 10),
			// Creating while the orchestrator still provisions (fulfil
			// 202); poll Describe* until it is Running/Available.
			"Status": c.ps.status(fr.State),
		}
		if created != nil {
			created(m, cp, fr)
		}
		return m, nil
	}
}

var imageIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ecsCreateParams(c *call, _ *createParams) error {
	// ImageId is accepted (svc-api-meta lists it, the Terraform provider
	// sends it) and checked for shape; the orchestrator has no image
	// attribute yet, so it is not recorded.
	img, err := c.p.str("ImageId")
	if err != nil {
		return err
	}
	if img != "" && !imageIDPattern.MatchString(img) {
		return invalidParam("ImageId", "is not an image id")
	}
	return nil
}

func rdsCreateParams(c *call, cp *createParams) error {
	engine, err := c.p.str("Engine")
	if err != nil {
		return err
	}
	if engine != "" && !strings.EqualFold(engine, "MySQL") {
		return invalidParam("Engine", "must be MySQL")
	}
	v, err := c.p.str("EngineVersion")
	if err != nil {
		return err
	}
	if v != "" {
		cp.attrs["engineVersion"] = v // "5.7" | "8.0", validated by the orchestrator
	}
	return nil
}

func vpcCreateParams(c *call, cp *createParams) error {
	cidr, err := c.p.need("CidrBlock")
	if err != nil {
		return err
	}
	cp.attrs["cidrBlock"] = cidr
	return nil
}

func diskCreateParams(c *call, cp *createParams) error {
	size, err := c.p.integer("Size", 0, 1, 1<<20)
	if err != nil {
		return err
	}
	if size == 0 {
		return missingParam("Size")
	}
	cp.attrs["sizeGb"] = strconv.FormatInt(size, 10) // GiB; range checked by the orchestrator
	return nil
}

func eipCreateParams(c *call, cp *createParams) error {
	bw, err := c.p.integer("Bandwidth", 0, 1, 1<<20)
	if err != nil {
		return err
	}
	if bw > 0 {
		cp.attrs["bandwidthMbps"] = strconv.FormatInt(bw, 10)
	}
	return nil
}

// bucketNamePattern is the coarse shape of a bucket name; the orchestrator
// applies the full rule set (no "..", no IP form, reserved prefixes).
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// bucketName reads the bucket parameter: Bucket (Terraform, OSS style) or
// BucketName (svc-api-meta's catalogue).
func bucketName(c *call) (string, error) {
	name, err := c.p.need("Bucket", "BucketName")
	if err != nil {
		return "", err
	}
	if !bucketNamePattern.MatchString(name) {
		return "", errorsx.New("InvalidBucketName", http.StatusBadRequest,
			"The bucket name must be 3-63 characters of lowercase letters, digits, dots and hyphens, starting and ending with a letter or digit.")
	}
	return name, nil
}

func bucketCreateParams(c *call, cp *createParams) error {
	name, err := bucketName(c)
	if err != nil {
		return err
	}
	// svc-api-meta documents StorageClass Standard|IA|Archive; the catalogue
	// sells Standard only (euoss.standard.*).
	class, err := c.p.str("StorageClass")
	if err != nil {
		return err
	}
	if class != "" && !strings.EqualFold(class, "Standard") {
		return invalidParam("StorageClass", "only Standard is on sale")
	}
	cp.attrs["bucket"] = name
	return nil
}

// bucketCreated answers with the bucket name (Bucket; BucketName is what the
// Terraform provider reads) and keeps the resource id as ResourceId.
func bucketCreated(m map[string]any, cp *createParams, fr *fulfilResult) {
	m["Bucket"] = cp.attrs["bucket"]
	m["BucketName"] = cp.attrs["bucket"]
	m["ResourceId"] = fr.ResourceID
}

// --- describe ----------------------------------------------------------------

// page reads PageNumber/PageSize.
func (c *call) page() (number, size int64, err error) {
	if number, err = c.p.integer("PageNumber", 1, 1, maxPageNumber); err != nil {
		return 0, 0, err
	}
	if size, err = c.p.integer("PageSize", defaultPageSize, 1, maxPageSize); err != nil {
		return 0, 0, err
	}
	return number, size, nil
}

// paginate answers a Describe: one page of items plus the paging fields.
func paginate(listKey string, items []map[string]any, number, size int64) map[string]any {
	total := int64(len(items))
	from := (number - 1) * size
	if from > total {
		from = total
	}
	to := from + size
	if to > total {
		to = total
	}
	return map[string]any{
		listKey:      items[from:to],
		"TotalCount": total,
		"PageNumber": number,
		"PageSize":   size,
	}
}

// describe lists the caller's live resources of the product, newest first,
// filtered by id (idParam / idsParam) and RegionId. Data is always the page:
// {listKey: [...], TotalCount, PageNumber, PageSize} — also for one id (the
// Terraform provider reads the resource out of listKey).
func (s *server) describe(c *call) (any, error) {
	ids, err := c.p.ids(c.ps.idParam, c.ps.idsParam)
	if err != nil {
		return nil, err
	}
	region, err := c.p.str("RegionId")
	if err != nil {
		return nil, err
	}
	number, size, err := c.page()
	if err != nil {
		return nil, err
	}
	all, err := s.listResources(c)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt != all[j].CreatedAt {
			return all[i].CreatedAt > all[j].CreatedAt
		}
		return all[i].ResourceID < all[j].ResourceID
	})
	items := make([]map[string]any, 0, len(all))
	for i := range all {
		r := &all[i]
		if (len(want) > 0 && !want[r.ResourceID]) || (region != "" && r.Region != region) {
			continue
		}
		items = append(items, c.ps.view(c.ps, r))
	}
	return paginate(c.ps.listKey, items, number, size), nil
}

// --- resource-addressed actions ----------------------------------------------

// power runs start|stop|restart on one resource.
func (s *server) power(action string) actionFunc {
	return func(c *call) (any, error) {
		id, err := c.p.need(c.ps.idParam)
		if err != nil {
			return nil, err
		}
		if _, err := s.resource(c, id); err != nil {
			return nil, err
		}
		state, err := s.resourceAction(c, id, action, "")
		if err != nil {
			return nil, err
		}
		return map[string]any{c.ps.idParam: id, "Status": c.ps.status(state)}, nil
	}
}

// modifySpec changes a Postpaid resource's spec (InstanceType /
// InstanceClass, or SkuCode) within its charge type. Prepaid needs an upgrade
// order the platform does not offer yet (OperationDenied.UpgradeRequiresOrder).
func (s *server) modifySpec(c *call) (any, error) {
	id, err := c.p.need(c.ps.idParam)
	if err != nil {
		return nil, err
	}
	sku, err := c.p.str("SkuCode")
	if err != nil {
		return nil, err
	}
	spec, err := c.p.str(c.ps.specParam)
	if err != nil {
		return nil, err
	}
	spec = strings.ToLower(spec)
	switch {
	case sku != "" && spec != "":
		return nil, invalidParam("SkuCode", "must not be combined with "+c.ps.specParam)
	case sku == "" && spec == "":
		return nil, missingParam(c.ps.specParam)
	case sku != "" && !skuCodePattern.MatchString(sku):
		return nil, invalidParam("SkuCode", "must be a SKU code such as euecs.s2.large.postpaid")
	case spec != "" && !specPattern.MatchString(spec):
		return nil, invalidParam(c.ps.specParam, "is not a specification name")
	}
	r, err := s.resource(c, id)
	if err != nil {
		return nil, err
	}
	target := sku
	c.specParam = "SkuCode"
	if spec != "" {
		target = skuFor(c.product, spec, r.ChargeType)
		c.specParam = c.ps.specParam
	}
	all, err := s.skus(c)
	if err != nil {
		return nil, err
	}
	var chosen *catalogSKU
	for i := range all {
		if all[i].SKUCode == target && all[i].ProductCode == c.product {
			chosen = &all[i]
		}
	}
	switch {
	case chosen == nil:
		return nil, c.specNotFound()
	case chosen.ChargeType != r.ChargeType:
		return nil, invalidParam("SkuCode", "must have the charge type of the resource ("+apiChargeType(r.ChargeType)+")")
	case chosen.Status != skuStatusOnSale:
		return nil, c.specNotOnSale()
	}
	state := r.State
	if target != r.SpecCode {
		if state, err = s.resourceAction(c, id, "upgrade", target); err != nil {
			return nil, err
		}
	}
	return map[string]any{c.ps.idParam: id, "SkuCode": target, "Status": c.ps.status(state)}, nil
}

// rename sets the display name (InstanceName / VpcName / DiskName).
func (s *server) rename(c *call) (any, error) {
	id, err := c.p.need(c.ps.idParam)
	if err != nil {
		return nil, err
	}
	name, err := c.p.name(c.ps.nameParam)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, missingParam(c.ps.nameParam)
	}
	if _, err := s.resource(c, id); err != nil {
		return nil, err
	}
	r, err := s.patchAttributes(c, id, map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	return map[string]any{c.ps.idParam: id, c.ps.nameParam: r.Attributes["name"]}, nil
}

// release deletes one resource (idParam) or several (idsParam). Every id is
// checked to be the caller's before any is released.
func (s *server) release(c *call) (any, error) {
	ids, err := c.p.ids(c.ps.idParam, c.ps.idsParam)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, missingParam(c.ps.idParam)
	}
	for _, id := range ids {
		if _, err := s.resource(c, id); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if err := s.releaseResource(c, id); err != nil {
			return nil, err
		}
	}
	m := map[string]any{}
	if len(ids) == 1 {
		m[c.ps.idParam] = ids[0]
	}
	if c.ps.idsParam != "" {
		m[c.ps.idsParam] = ids
	}
	return m, nil
}

// --- euoss control plane -----------------------------------------------------

// listBuckets lists the caller's buckets (optionally by Prefix), by name.
func (s *server) listBuckets(c *call) (any, error) {
	prefix, err := c.p.str("Prefix")
	if err != nil {
		return nil, err
	}
	number, size, err := c.page()
	if err != nil {
		return nil, err
	}
	all, err := s.listResources(c)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Attributes["bucket"] < all[j].Attributes["bucket"] })
	items := make([]map[string]any, 0, len(all))
	for i := range all {
		if strings.HasPrefix(all[i].Attributes["bucket"], prefix) {
			items = append(items, c.ps.view(c.ps, &all[i]))
		}
	}
	return paginate(c.ps.listKey, items, number, size), nil
}

// deleteBucket releases the caller's bucket of that name.
func (s *server) deleteBucket(c *call) (any, error) {
	name, err := bucketName(c)
	if err != nil {
		return nil, err
	}
	all, err := s.listResources(c)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.Attributes["bucket"] == name {
			if err := s.releaseResource(c, r.ResourceID); err != nil {
				return nil, err
			}
			return map[string]any{"Bucket": name}, nil
		}
	}
	return nil, c.notFound()
}
