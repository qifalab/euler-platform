package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/autoscaling"
	"github.com/qifalab/euler-platform/backup"
	"github.com/qifalab/euler-platform/ipam"
	"github.com/qifalab/euler-platform/resource"
)

// Resource attributes (resource_instance_attr): the customer-facing creation
// parameters of a resource — given as the fulfil body's `params`, validated
// per product, stored with the row, rendered as `attributes` on the detail and
// list records and, for the mutable keys, changed with
// PATCH /api/v1/orchestrator/resources/{id}/attributes.
//
// Keys under sysAttrPrefix are the orchestrator's own bookkeeping on the same
// table. They are never accepted from a caller (no rule names them) and never
// rendered (publicAttrs).

// Attribute keys.
const (
	attrName          = "name"
	attrCIDRBlock     = "cidrBlock"
	attrEngineVersion = "engineVersion"
	attrBucket        = "bucket"
	attrSizeGB        = "sizeGb"
	attrBandwidthMbps = "bandwidthMbps"
	attrMinSize       = "minSize"
	attrMaxSize       = "maxSize"
	attrDesired       = "desiredCapacity"
	attrCooldown      = "cooldownSeconds"
	attrRetentionDays = "retentionDays"
	attrSchedule      = "schedule"
	attrStorageGB     = "storageGb"

	// sysAttrPrefix marks the orchestrator's bookkeeping attributes.
	sysAttrPrefix = "sys."
	// attrCompensating is set (to the reason) while a CREATING resource's
	// compensation is claimed: no settle may deliver it (MarkCompensation).
	attrCompensating = "sys.compensating"
	// attrQuotaReservation is the svc-quota reservation the resource was
	// provisioned under; attrQuotaState tracks it (quotaOccupied/Committed/
	// Released) so a replay can finish a commit or release that failed.
	attrQuotaReservation = "sys.quotaReservation"
	attrQuotaState       = "sys.quotaState"
	// attrTermDuration/attrTermUnit are the purchased prepaid term, so whoever
	// delivers the resource (the fulfil, a replay, the poller) stamps the same
	// expiry without asking svc-order again.
	attrTermDuration = "sys.termDuration"
	attrTermUnit     = "sys.termUnit"
)

// Attribute value limits.
const (
	nameMaxRunes       = 64
	vpcPrefixMin       = 16
	vpcPrefixMax       = 28
	bucketMinLen       = 3
	bucketMaxLen       = 63
	ebsSizeMinGB       = 20
	ebsSizeMaxGB       = 32768
	eipBandwidthMin    = 1
	eipBandwidthMax    = 1000
	asgSizeMax         = 1000
	asgCooldownMax     = 86400
	retentionDaysMin   = 1
	retentionDaysMax   = 3650
	logStorageMaxGB    = 1 << 20 // 1 PiB in GB: a sanity cap, not a sales limit
	defaultBackupKeepD = 30      // eubackup snapshots without a retentionDays attribute
)

// rdsEngineVersions are the EURDS engine versions the catalogue sells.
var rdsEngineVersions = []string{"5.7", "8.0"}

// apiError is a request failure with its envelope code and HTTP status.
type apiError struct {
	Code   string
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Msg }

func writeAPIError(w http.ResponseWriter, e *apiError) { writeErr(w, e.Code, e.Status, e.Msg) }

func invalidAttr(format string, args ...any) *apiError {
	return &apiError{Code: "Resource.InvalidAttribute", Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

// attrRule is one attribute key of a product.
type attrRule struct {
	required bool // must be given at creation
	mutable  bool // may be changed by the attributes PATCH
	// canon validates a value and returns its canonical form (what is
	// stored), or an error message.
	canon func(v string) (string, error)
}

// commonAttrRules apply to every product.
var commonAttrRules = map[string]attrRule{
	attrName: {mutable: true, canon: canonName},
}

// productAttrRules are the product-specific keys (O1). Products not listed
// take only the common keys.
var productAttrRules = map[string]map[string]attrRule{
	"euvpc": {
		attrCIDRBlock: {required: true, canon: canonVPCBlock},
	},
	"eurds": {
		attrEngineVersion: {canon: canonOneOf(rdsEngineVersions...)},
	},
	"euoss": {
		attrBucket: {required: true, canon: canonBucket},
	},
	"eubs": {
		attrSizeGB: {canon: canonInt(ebsSizeMinGB, ebsSizeMaxGB)},
	},
	"eueip": {
		attrBandwidthMbps: {canon: canonInt(eipBandwidthMin, eipBandwidthMax)},
	},
	"euas": {
		attrMinSize:  {mutable: true, canon: canonInt(0, asgSizeMax)},
		attrMaxSize:  {mutable: true, canon: canonInt(0, asgSizeMax)},
		attrDesired:  {mutable: true, canon: canonInt(0, asgSizeMax)},
		attrCooldown: {mutable: true, canon: canonInt(0, asgCooldownMax)},
	},
	"eubackup": {
		attrRetentionDays: {mutable: true, canon: canonInt(retentionDaysMin, retentionDaysMax)},
		attrSchedule:      {mutable: true, canon: canonSchedule},
	},
	"eulog": {
		attrStorageGB:     {mutable: true, canon: canonInt(1, logStorageMaxGB)},
		attrRetentionDays: {mutable: true, canon: canonInt(retentionDaysMin, retentionDaysMax)},
	},
}

func attrRuleFor(product, key string) (attrRule, bool) {
	if r, ok := commonAttrRules[key]; ok {
		return r, true
	}
	r, ok := productAttrRules[product][key]
	return r, ok
}

// attrValues is a {string:string} request map that also takes JSON numbers
// (stringified as written): {"sizeGb": 100} and {"sizeGb": "100"} mean the
// same. Anything else (objects, arrays, booleans) is a malformed body.
type attrValues map[string]string

func (m *attrValues) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(attrValues, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return fmt.Errorf("attribute %q: want a string or a number", k)
		}
		out[k] = n.String()
	}
	*m = out
	return nil
}

// validateCreateParams checks fulfil params for a new resource of product and
// returns the canonical attributes to store: every key must be known for the
// product, every value valid, the required keys present and the keys
// consistent with each other.
func validateCreateParams(product string, params attrValues) (map[string]string, *apiError) {
	out := make(map[string]string, len(params))
	for _, k := range sortedKeys(params) {
		rule, ok := attrRuleFor(product, k)
		if !ok {
			return nil, invalidAttr("产品 %s 不支持参数 %s", product, k)
		}
		v, err := rule.canon(params[k])
		if err != nil {
			return nil, invalidAttr("参数 %s 不合法: %v", k, err)
		}
		out[k] = v
	}
	for _, rules := range []map[string]attrRule{commonAttrRules, productAttrRules[product]} {
		for _, k := range sortedRuleKeys(rules) {
			if _, given := out[k]; rules[k].required && !given {
				return nil, invalidAttr("产品 %s 缺少必填参数 %s", product, k)
			}
		}
	}
	if err := crossCheckAttrs(product, out); err != nil {
		return nil, err
	}
	return out, nil
}

// validatePatch checks a PATCH body: only mutable keys (anything else —
// immutable or unknown — is Resource.AttributeImmutable), each value valid.
// The cross-key check runs later against the merged attributes, under the
// ledger's row lock.
func validatePatch(product string, set attrValues) (map[string]string, *apiError) {
	out := make(map[string]string, len(set))
	for _, k := range sortedKeys(set) {
		rule, ok := attrRuleFor(product, k)
		if !ok || !rule.mutable {
			return nil, &apiError{Code: "Resource.AttributeImmutable", Status: http.StatusBadRequest,
				Msg: fmt.Sprintf("参数 %s 创建后不可修改", k)}
		}
		v, err := rule.canon(set[k])
		if err != nil {
			return nil, invalidAttr("参数 %s 不合法: %v", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// crossCheckAttrs validates keys that only make sense together: the euas
// scaling policy (pkg-go/autoscaling). Missing policy keys take the values
// that impose nothing — minSize 0, desiredCapacity = minSize, maxSize =
// max(minSize, desiredCapacity) — so a partial policy is judged on what it
// says.
func crossCheckAttrs(product string, attrs map[string]string) *apiError {
	if product != "euas" {
		return nil
	}
	_, hasMin := attrs[attrMinSize]
	_, hasMax := attrs[attrMaxSize]
	_, hasDesired := attrs[attrDesired]
	_, hasCooldown := attrs[attrCooldown]
	if !hasMin && !hasMax && !hasDesired && !hasCooldown {
		return nil
	}
	num := func(k string, def int) int {
		if v, ok := attrs[k]; ok {
			n, _ := strconv.Atoi(v) // canonical: validated on the way in
			return n
		}
		return def
	}
	p := autoscaling.ScalingPolicy{MinReplicas: num(attrMinSize, 0)}
	p.DesiredReplicas = num(attrDesired, p.MinReplicas)
	p.MaxReplicas = num(attrMaxSize, max(p.MinReplicas, p.DesiredReplicas))
	p.CooldownSeconds = num(attrCooldown, 0)
	if err := p.Validate(); err != nil {
		return invalidAttr("伸缩策略不合法(minSize %d, maxSize %d, desiredCapacity %d): %v",
			p.MinReplicas, p.MaxReplicas, p.DesiredReplicas, err)
	}
	return nil
}

func sortedRuleKeys(m map[string]attrRule) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// publicAttrs is what the API renders: everything but the system keys. Never
// nil, so the JSON is always an object.
func publicAttrs(attrs map[string]string) map[string]string {
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if !strings.HasPrefix(k, sysAttrPrefix) {
			out[k] = v
		}
	}
	return out
}

// attributesWritable reports whether a resource in state takes attribute
// changes: not once it is being or has been released, nor after its creation
// failed (it was rolled back; only release remains).
func attributesWritable(st resource.State) bool {
	return st != resource.StateReleasing && st != resource.StateReleased && st != resource.StateCreateFailed
}

// --- value rules ----------------------------------------------------------------

func canonName(v string) (string, error) {
	v = strings.TrimSpace(v)
	if n := utf8.RuneCountInString(v); n < 1 || n > nameMaxRunes {
		return "", fmt.Errorf("名称长度须为 1-%d 个字符", nameMaxRunes)
	}
	if !utf8.ValidString(v) {
		return "", errors.New("名称不是合法的 UTF-8")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errors.New("名称不能包含控制字符")
		}
	}
	return v, nil
}

// canonInt accepts a plain decimal integer in [min, max] (no sign, no
// fraction) and returns it without leading zeros.
func canonInt(min, max int) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > 10 || strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return "", fmt.Errorf("须为 %d-%d 之间的整数", min, max)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			return "", fmt.Errorf("须为 %d-%d 之间的整数", min, max)
		}
		return strconv.Itoa(n), nil
	}
}

func canonOneOf(allowed ...string) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.TrimSpace(v)
		for _, a := range allowed {
			if v == a {
				return v, nil
			}
		}
		return "", fmt.Errorf("可选值为 %s", strings.Join(allowed, ", "))
	}
}

// canonVPCBlock: an RFC 1918 IPv4 block, /16../28, without host bits
// (pkg-go/ipam). Overlap with the account's other VPCs is checked at create
// time (vpcOverlapCheck), where the ledger can make it atomic.
func canonVPCBlock(v string) (string, error) {
	c, err := ipam.ParseCIDR(strings.TrimSpace(v))
	if err != nil {
		return "", errors.New("须为 a.b.c.d/n 形式的 IPv4 网段,且主机位为 0")
	}
	if !c.IsRFC1918() {
		return "", errors.New("须位于私网地址段 10.0.0.0/8、172.16.0.0/12 或 192.168.0.0/16 内")
	}
	if c.Bits() < vpcPrefixMin || c.Bits() > vpcPrefixMax {
		return "", fmt.Errorf("掩码长度须为 /%d-/%d", vpcPrefixMin, vpcPrefixMax)
	}
	return c.String(), nil
}

// vpcOverlapCheck refuses a VPC block overlapping one of the account's live
// VPCs in the region. A peer without a recorded block (the dev seed, VPCs from
// before O1) reserves no range.
func vpcOverlapCheck(block string) func([]peerResource) error {
	return func(peers []peerResource) error {
		mine, err := ipam.ParseCIDR(block)
		if err != nil {
			return invalidAttr("参数 cidrBlock 不合法: %v", err)
		}
		for _, p := range peers {
			other, err := ipam.ParseCIDR(p.Attrs[attrCIDRBlock])
			if err != nil {
				continue
			}
			if mine.Overlaps(other) {
				return &apiError{Code: "Resource.InvalidAttribute", Status: http.StatusConflict,
					Msg: fmt.Sprintf("网段 %s 与本地域 VPC %s 的网段 %s 重叠", mine, p.ResourceID, other)}
			}
		}
		return nil
	}
}

// canonBucket applies the S3 bucket naming rules: 3-63 characters of
// lowercase letters, digits, dots and hyphens; starts and ends with a letter
// or digit; no "..", ".-" or "-."; not an IPv4 address; none of S3's reserved
// prefixes and suffixes.
func canonBucket(v string) (string, error) {
	if len(v) < bucketMinLen || len(v) > bucketMaxLen {
		return "", fmt.Errorf("桶名长度须为 %d-%d 个字符", bucketMinLen, bucketMaxLen)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return "", errors.New("桶名只能包含小写字母、数字、点和连字符")
		}
	}
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
	if !alnum(v[0]) || !alnum(v[len(v)-1]) {
		return "", errors.New("桶名须以字母或数字开头和结尾")
	}
	if strings.Contains(v, "..") || strings.Contains(v, ".-") || strings.Contains(v, "-.") {
		return "", errors.New("桶名不能包含相邻的点,或点与连字符相邻")
	}
	if ip, err := netip.ParseAddr(v); err == nil && ip.Is4() {
		return "", errors.New("桶名不能是 IP 地址格式")
	}
	for _, p := range []string{"xn--", "sthree-"} {
		if strings.HasPrefix(v, p) {
			return "", fmt.Errorf("桶名不能以 %s 开头", p)
		}
	}
	for _, s := range []string{"-s3alias", "--ol-s3"} {
		if strings.HasSuffix(v, s) {
			return "", fmt.Errorf("桶名不能以 %s 结尾", s)
		}
	}
	return v, nil
}

// canonSchedule: "daily HH:MM" (bare "HH:MM" accepted) or "every Nh"
// (pkg-go/backup.ParseSchedule), stored in canonical form.
func canonSchedule(v string) (string, error) {
	s, err := backup.ParseSchedule(v)
	if err != nil {
		return "", fmt.Errorf("须为 \"daily HH:MM\" 或 \"every Nh\"(N 为 1-%d)", backup.MaxScheduleHours)
	}
	return s.String(), nil
}

// --- PATCH /api/v1/orchestrator/resources/{id}/attributes ----------------------

type patchAttributesRequest struct {
	Attributes attrValues `json:"attributes"`
}

// handlePatchAttributes changes a live resource's mutable attributes (O2).
// The write, its validation against the merged attributes and its journal row
// happen under the ledger's row lock; the answer is the updated detail.
func (s *lifecycleStore) handlePatchAttributes(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req patchAttributesRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if len(req.Attributes) == 0 {
		writeErr(w, "Common.InvalidParameter", 400, "attributes is required")
		return
	}
	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	set, aerr := validatePatch(inst.ProductCode, req.Attributes)
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}
	changes := make([]string, 0, len(set))
	for _, k := range sortedKeys(set) {
		changes = append(changes, k+"="+set[k])
	}
	err = s.ledger.UpdateAttrs(acct, resID, attrUpdate{
		Set: set,
		Check: func(st resource.State, current map[string]string) error {
			if !attributesWritable(st) {
				return &apiError{Code: "Resource.InvalidState", Status: http.StatusConflict,
					Msg: fmt.Sprintf("资源状态 %s 不允许修改属性", st)}
			}
			merged := cloneAttrs(current)
			for k, v := range set {
				merged[k] = v
			}
			if e := crossCheckAttrs(inst.ProductCode, merged); e != nil {
				return e
			}
			return nil
		},
		Journal: "attributes updated: " + strings.Join(changes, ", "),
	})
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		writeAPIError(w, ae)
		return
	case errors.Is(err, errResourceNotFound):
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	case err != nil:
		slog.Error("resource attribute update failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.UpdateFailed", 500, "资源属性更新失败")
		return
	}
	s.writeResourceDetail(w, acct, resID)
}
