/**
 * @vitest-environment node
 */
import { describe, it, expect } from "vitest";
import { canRunAction, defaultSpecLabel, parseSpec, upgradeOptions, type CatalogSku } from "./resourceActions";

describe("canRunAction (svc-orchestrator rules)", () => {
  it("start only from STOPPED; stop/restart only from RUNNING", () => {
    expect(canRunAction("start", "STOPPED")).toBe(true);
    expect(canRunAction("start", "RUNNING")).toBe(false);
    expect(canRunAction("start", "LOCKED")).toBe(false);
    expect(canRunAction("stop", "RUNNING")).toBe(true);
    expect(canRunAction("restart", "RUNNING")).toBe(true);
    expect(canRunAction("restart", "STOPPED")).toBe(false);
    expect(canRunAction("stop", "CREATING")).toBe(false);
  });

  it("upgrade only for a running postpaid instance", () => {
    expect(canRunAction("upgrade", "RUNNING", "POSTPAID")).toBe(true);
    expect(canRunAction("upgrade", "RUNNING", "PREPAID")).toBe(false);
    expect(canRunAction("upgrade", "STOPPED", "POSTPAID")).toBe(false);
  });

  it("release also cleans up CREATE_FAILED / LOCKED / EXPIRED; renew does not", () => {
    for (const s of ["CREATE_FAILED", "LOCKED", "EXPIRED"]) {
      expect(canRunAction("release", s)).toBe(true);
      expect(canRunAction("renew", s)).toBe(false);
    }
    expect(canRunAction("release", "RELEASED")).toBe(false);
    expect(canRunAction("release", "UPGRADING")).toBe(false);
  });

  it("release / renew for RUNNING or STOPPED, never mid-transition", () => {
    for (const a of ["release", "renew"] as const) {
      expect(canRunAction(a, "RUNNING")).toBe(true);
      expect(canRunAction(a, "STOPPED")).toBe(true);
      expect(canRunAction(a, "CREATING")).toBe(false);
      expect(canRunAction(a, "RELEASING")).toBe(false);
      expect(canRunAction(a, "")).toBe(false);
    }
  });
});

describe("upgradeOptions", () => {
  const sku = (skuCode: string, chargeType: string, specJson: string, status = "1"): CatalogSku => ({
    skuCode, productCode: skuCode.split(".")[0], chargeType, specJson, status,
  });
  const skus = [
    sku("euredis.redis.small.postpaid", "POSTPAID", '{"mem_gb":1}'),
    sku("euredis.redis.large.postpaid", "POSTPAID", '{"mem_gb":4}'),
    sku("euredis.redis.large.prepaid", "PREPAID", '{"mem_gb":4}'),
    sku("euredis.redis.xl.postpaid", "POSTPAID", '{"mem_gb":8}', "0"),
    sku("eukafka.kafka.standard.postpaid", "POSTPAID", '{"broker_count":3}'),
  ];

  it("keeps on-sale SKUs of the same product and charge type, minus the current spec", () => {
    const opts = upgradeOptions(
      skus,
      { productCode: "euredis", chargeType: "POSTPAID", specCode: "euredis.redis.small.postpaid" },
      (s) => `Redis ${String(s.mem_gb)}G`,
    );
    expect(opts).toEqual([{ skuCode: "euredis.redis.large.postpaid", label: "Redis 4G" }]);
  });

  it("falls back to a generic label", () => {
    expect(defaultSpecLabel(parseSpec('{"type":"l7","max_qps":100}'), skus[0])).toBe("type=l7 · max_qps=100");
    expect(defaultSpecLabel(parseSpec("not json"), skus[0])).toBe("euredis.redis.small.postpaid");
  });
});
