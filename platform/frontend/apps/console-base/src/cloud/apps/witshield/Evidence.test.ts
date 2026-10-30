import { mount } from "@vue/test-utils";
import { describe, it, expect } from "vitest";
import Evidence from "./Evidence.vue";

describe("WitShield evidence labels", () => {
  it("labels the scanner report summary by the fields the engine sends", () => {
    // Shape written by internal/scanner and validated on report upload.
    const wrapper = mount(Evidence, {
      props: {
        value: {
          checks: 42,
          completedChecks: 40,
          coveragePercent: 95,
          findingCount: 3,
          checkErrors: ["journal unavailable", "apt locked"],
          mode: "native",
        },
      },
    });
    const labels = wrapper.findAll("dt").map((dt) => dt.text());
    expect(labels).toEqual([
      "检查项",
      "完成检查",
      "检查覆盖率",
      "发现数量",
      "检查错误",
      "权限/模式",
    ]);
    wrapper.unmount();
  });
});
