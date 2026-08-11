package identifier

import "testing"

func TestResourceIDRoundTrip(t *testing.T) {
	const raw = "scecs-cn-north-1-01-a1b2c3d4"
	rid, err := ParseResourceID(raw)
	if err != nil {
		t.Fatalf("ParseResourceID: %v", err)
	}
	if rid.ProductCode != "scecs" || rid.RegionID != "cn-north-1" ||
		rid.ShardFactor != "01" || rid.Random != "a1b2c3d4" {
		t.Fatalf("parsed fields wrong: %+v", rid)
	}
	if rid.String() != raw {
		t.Fatalf("String() round-trip = %q, want %q", rid.String(), raw)
	}
	dbIdx, tblIdx, err := rid.ShardIndices()
	if err != nil {
		t.Fatalf("ShardIndices: %v", err)
	}
	if dbIdx != 0 || tblIdx != 1 {
		t.Fatalf("ShardIndices = (%d,%d), want (0,1)", dbIdx, tblIdx)
	}
}

func TestParseResourceIDRejectsBad(t *testing.T) {
	cases := []string{
		"",                         // empty
		"scecs-cn-north-1-01-",     // missing random
		"scecs-cnnorth1-01-a1b2c3d4", // region not hyphen-style
		"SCECS-cn-north-1-01-a1b2c3d4", // uppercase product
		"scecs-cn-north-1-1-a1b2c3d4",  // 1-digit shard
		"scecs-cn-north-1-01-A1B2C3D4", // uppercase random
	}
	for _, c := range cases {
		if _, err := ParseResourceID(c); err == nil {
			t.Fatalf("ParseResourceID(%q) should fail", c)
		}
	}
}

func TestShardFactorFor(t *testing.T) {
	// account_id=100123 → 100123 % 8 = 3, 100123 % 16 = 11 → 11 % 10 = 1 → "31"
	// (each digit is single-decimal via the mod-10 collapse so the factor
	// always fits in 2 chars; see 04§6.6).
	got := ShardFactorFor(100123, 8, 16)
	want := "31"
	if got != want {
		t.Fatalf("ShardFactorFor(100123,8,16) = %q, want %q", got, want)
	}
}

func TestNewResourceID(t *testing.T) {
	rid, err := NewResourceID("scecs", "cn-north-1", 100123, 8, 16)
	if err != nil {
		t.Fatalf("NewResourceID: %v", err)
	}
	if rid.ShardFactor != "31" {
		t.Fatalf("ShardFactor = %q, want 31", rid.ShardFactor)
	}
	// Random must be 8 hex chars.
	if len(rid.Random) != 8 {
		t.Fatalf("Random len = %d, want 8", len(rid.Random))
	}
	// Round-trips through the parser.
	if _, err := ParseResourceID(rid.String()); err != nil {
		t.Fatalf("generated id failed to parse: %v", err)
	}
}

func TestRegionAndAZ(t *testing.T) {
	if !IsValidRegion("cn-north-1") {
		t.Fatal("cn-north-1 should be valid")
	}
	if IsValidRegion("cnnorth1") {
		t.Fatal("cnnorth1 should be invalid")
	}
	if got := AZName("cn-north-1", "a"); got != "cn-north-1-a" {
		t.Fatalf("AZName = %q", got)
	}
}

func TestNaming(t *testing.T) {
	if got := ServiceName("iam"); got != "svc-iam" {
		t.Fatalf("ServiceName = %q", got)
	}
	if got := BFFName("console"); got != "console-bff" {
		t.Fatalf("BFFName = %q", got)
	}
	if got := OpenAPIDomain("scecs"); got != "scecs.api.starcloud.cn" {
		t.Fatalf("OpenAPIDomain = %q", got)
	}
	if got := ConsoleRoute("scecs"); got != "/console/scecs" {
		t.Fatalf("ConsoleRoute = %q", got)
	}
	if got := PermissionAction("scecs", "CreateInstance"); got != "scecs:CreateInstance" {
		t.Fatalf("PermissionAction = %q", got)
	}
	if got := ARN("ecs", "cn-east-1", 100123, "instance/scecs-cn-east-1-01-a1b2c3d4"); got != "sc:ecs:cn-east-1:100123:instance/scecs-cn-east-1-01-a1b2c3d4" {
		t.Fatalf("ARN = %q", got)
	}
	if got := ErrorCode("Quota", "Exceeded", "ScecsInstance"); got != "Quota.Exceeded.ScecsInstance" {
		t.Fatalf("ErrorCode = %q", got)
	}
	if got := KafkaTopic("metering", "usage", "raw"); got != "cloud.metering.usage.raw" {
		t.Fatalf("KafkaTopic = %q", got)
	}
	if got := SystemKafkaTopic("audit.action"); got != "cloud.sys.audit.action" {
		t.Fatalf("SystemKafkaTopic = %q", got)
	}
	if got := AKID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); len(got) != 32 || got[:2] != "SC" {
		t.Fatalf("AKID = %q", got)
	}
	if !IsValidProductCode("scecs") || IsValidProductCode("ecs") || IsValidProductCode("SCECS") {
		t.Fatalf("IsValidProductCode wrong")
	}
	if got := NacosSubscriptionString("svc-order"); got != "svc-order@@svc-order" {
		t.Fatalf("NacosSubscriptionString = %q", got)
	}
	if got := NacosGroup("svc-order"); got != "svc-order" {
		t.Fatalf("NacosGroup = %q", got)
	}
	if got := SystemPolicyName("Ecs", "FullAccess"); got != "ScEcsFullAccess" {
		t.Fatalf("SystemPolicyName = %q", got)
	}
}
