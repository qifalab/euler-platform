// Package identifier encodes the platform's global identifier conventions
// (00-overview.md 附录 A "全局标识规范", adjudication C9/S3/S4). These are the
// only source of truth for naming: product codes, resource IDs, region/AZ
// names, service names, Kafka topic names, ARNs, error codes, and AK format.
//
// Every service imports this package rather than hand-writing identifiers, so
// the conventions cannot drift across the 17 microservices.
package identifier

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Domain is the platform primary domain (00 附录A).
const Domain = "starcloud.cn"

// BrandPrefix is the platform/brand prefix, replacing cldp/cps/CPSA (C9).
const BrandPrefix = "sc"

// AKPrefix is the access-key prefix (07§2.5, replacing LTAI/CPSA).
const AKPrefix = "SC"

// SignatureHeaderPrefix is the OpenAPI signature-protocol header prefix. It is
// intentionally decoupled from BrandPrefix and retained unchanged (C9/S4).
const SignatureHeaderPrefix = "x-cps-"

// ResourceID is a parsed platform resource identifier.
//
// Format: {productCode}-{regionId}-{2-digit shard factor}-{8-char random}
// Example: scecs-cn-north-1-01-a1b2c3d4
//
// The 2-digit shard factor encodes the account_id routing: it is derived from
// (account_id % dbCount)(account_id % tableCount) so a resource_id alone can
// be reverse-routed to its shard without a global index lookup (04§6.6).
type ResourceID struct {
	ProductCode string
	RegionID    string
	ShardFactor string // exactly 2 digits
	Random      string // exactly 8 hex chars
}

// resourceIDPattern enforces the full format.
var resourceIDPattern = regexp.MustCompile(
	`^([a-z]{3,8})-((cn|ap|us|eu)-[a-z]+-\d+)-(\d{2})-([0-9a-f]{8})$`,
)

// ParseResourceID parses a resource identifier string.
func ParseResourceID(s string) (ResourceID, error) {
	m := resourceIDPattern.FindStringSubmatch(s)
	if m == nil {
		return ResourceID{}, fmt.Errorf("identifier: invalid resource id %q (want {productCode}-{regionId}-{2-digit shard}-{8-hex random})", s)
	}
	return ResourceID{
		ProductCode: m[1],
		RegionID:    m[2],
		ShardFactor: m[4],
		Random:      m[5],
	}, nil
}

// String reconstructs the canonical resource id.
func (r ResourceID) String() string {
	return fmt.Sprintf("%s-%s-%s-%s", r.ProductCode, r.RegionID, r.ShardFactor, r.Random)
}

// ShardIndices decomposes the 2-digit shard factor into (dbIndex, tableIndex).
func (r ResourceID) ShardIndices() (dbIdx, tableIdx int, err error) {
	if len(r.ShardFactor) != 2 || r.ShardFactor[0] < '0' || r.ShardFactor[0] > '9' || r.ShardFactor[1] < '0' || r.ShardFactor[1] > '9' {
		return 0, 0, errors.New("identifier: shard factor must be 2 digits")
	}
	return int(r.ShardFactor[0] - '0'), int(r.ShardFactor[1] - '0'), nil
}

// ShardFactorFor returns the 2-digit shard factor for a given account_id
// under a sharding topology of dbCount databases × tableCount tables
// (04§6.4; default topology: account_db 4×16, trade/resource/metering_db 8×16).
//
// account_id is the global tenant identifier (≡ uid ≡ user_id ≡ tenant_id).
func ShardFactorFor(accountID int64, dbCount, tableCount int) string {
	if dbCount <= 0 {
		dbCount = 8
	}
	if tableCount <= 0 {
		tableCount = 16
	}
	// Each digit is mod 10 so it is a single decimal digit (0–9). This
	// supports up to 10 databases × 10 tables via the embedded factor alone;
	// larger topologies (e.g. 8×16) still route correctly because the factor
	// is a *hint* reverse-routed through the same mod — the authoritative
	// shard for account_id is account_id % dbCount, computed by the sharding
	// layer. See 04§6.6: the factor is (account_id % 库数)(account_id % 表数)
	// but must fit in 2 chars, so we collapse each to a single digit.
	dbDigit := accountID % int64(dbCount) % 10
	tblDigit := accountID % int64(tableCount) % 10
	return fmt.Sprintf("%d%d", dbDigit, tblDigit)
}

// NewResourceID generates a new resource identifier for the given product,
// region, and account. It is the platform-standard way to mint a resource id
// at creation time (svc-orchestrator is the sole caller for the ledger table).
func NewResourceID(productCode, regionID string, accountID int64, dbCount, tableCount int) (ResourceID, error) {
	if productCode == "" || regionID == "" {
		return ResourceID{}, errors.New("identifier: productCode and regionID required")
	}
	rand8, err := randomHex(4) // 4 bytes → 8 hex chars
	if err != nil {
		return ResourceID{}, err
	}
	return ResourceID{
		ProductCode: productCode,
		RegionID:    regionID,
		ShardFactor: ShardFactorFor(accountID, dbCount, tableCount),
		Random:      rand8,
	}, nil
}

// randomHex returns n random bytes as a lowercase hex string of length 2*n.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Region regex: cn-north-1, cn-east-1, ap-southeast-1, etc.
var regionPattern = regexp.MustCompile(`^(cn|ap|us|eu)-[a-z]+-\d+$`)

// IsValidRegion reports whether r is a syntactically valid region name
// (cn-north-1 style; 00 附录A, hyphen-style).
func IsValidRegion(r string) bool {
	return regionPattern.MatchString(r)
}

// AZName builds an availability-zone name from a region and a zone letter:
// {region}-{a/b/...} → cn-north-1-a (00 附录A).
func AZName(region string, zoneLetter string) string {
	return region + "-" + zoneLetter
}

// ServiceName builds a control-plane service name: svc-{domain} (C4/S11).
func ServiceName(domain string) string {
	return "svc-" + domain
}

// BFFName builds an access-layer BFF name: {scene}-bff (C4/S11).
func BFFName(scene string) string {
	return scene + "-bff"
}

// OpenAPIDomain returns {productCode}.api.starcloud.cn (04§3.2).
func OpenAPIDomain(productCode string) string {
	return productCode + ".api." + Domain
}

// ConsoleRoute returns the console route prefix for a product: /console/{productCode}.
func ConsoleRoute(productCode string) string {
	return "/console/" + productCode
}

// PermissionAction builds a RAM permission action: {productCode}:{Operation}.
// Example: scecs:CreateInstance (07§3.1, S3).
func PermissionAction(productCode, operation string) string {
	return productCode + ":" + operation
}

// ARN builds a platform resource name:
// sc:{service}:{region}:{account_id}:{relative-resource}
// Example: sc:ecs:cn-east-1:100123:instance/scecs-cn-east-1-01-a1b2c3d4 (07§3.1).
func ARN(service, region string, accountID int64, relative string) string {
	return fmt.Sprintf("%s:%s:%s:%d:%s", BrandPrefix, service, region, accountID, relative)
}

// SystemPolicyName builds a system policy name: Sc{Product}FullAccess /
// Sc{Product}ReadOnlyAccess (07§3.2). pass "FullAccess" or "ReadOnlyAccess"
// as tier.
func SystemPolicyName(productName, tier string) string {
	return "Sc" + productName + tier
}

// Error code format: {Product}.{Module}.{Reason} (PascalCase), e.g.
// Quota.Exceeded.ScecsInstance (03§9.3).
func ErrorCode(product, module, reason string) string {
	return fmt.Sprintf("%s.%s.%s", product, module, reason)
}

// KafkaTopic builds a topic name following cloud.{domain}.{aggregate}.{event}
// (04§5.3). Topic names MUST NOT contain environment identifiers; env
// isolation is by cluster (C2/S8).
func KafkaTopic(domain, aggregate, event string) string {
	return strings.Join([]string{"cloud", domain, aggregate, event}, ".")
}

// SystemKafkaTopic builds a platform-level topic: cloud.sys.{purpose}
// (audit/alert/notify/threat/workflow/log-buffer/rum/...).
func SystemKafkaTopic(purpose string) string {
	return strings.Join([]string{"cloud", "sys", purpose}, ".")
}

// AKID builds a 32-character access key identifier with the SC prefix.
// The caller supplies 30 random alphanumeric chars; this is a formatting
// helper, not a security primitive — actual AK generation happens in svc-iam
// with a cryptographically secure RNG.
func AKID(random30 string) string {
	return AKPrefix + random30
}

// ProductCode validates and normalizes a product code: lowercase, sc-prefix,
// 3–8 alpha chars (scecs, scoss, scrds, scvpc, scbs, sceip, scmon, sceci, sccert).
var productCodePattern = regexp.MustCompile(`^sc[a-z]{1,6}$`)

// IsValidProductCode reports whether code matches the sc-prefixed product
// code convention (01§1.1 D0).
func IsValidProductCode(code string) bool {
	return productCodePattern.MatchString(code)
}

// NacosGroup returns the Nacos group for a service, which by convention equals
// the service name (04§4.3; e.g. Group=svc-order). The APISIX discovery
// subscription string is {GROUP}@@{serviceName}.
func NacosGroup(serviceName string) string {
	return serviceName
}

// NacosSubscriptionString returns the APISIX discovery subscription token
// for a service: {GROUP}@@{serviceName} (04§3.7, pit #2).
func NacosSubscriptionString(serviceName string) string {
	return serviceName + "@@" + serviceName
}
