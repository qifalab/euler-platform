/**
 * @vitest-environment jsdom
 *
 * Vitest tests for @eu/ui display components (02§7.2, 02§10.3):
 *   - StatusBadge: status→kind mapping + label override
 *   - PriceText:   minor-unit / decimal-string money formatting + currency symbol
 *
 * Co-located in packages/ui/src; vitest auto-discovers *.test.ts. No real
 * network or timers are involved — these are pure presentational components,
 * so we only mount them via @vue/test-utils and assert rendered DOM/text.
 */
import { describe, it, expect } from "vitest";
import { mount } from "@vue/test-utils";
import StatusBadge from "./StatusBadge.vue";
import PriceText from "./PriceText.vue";

describe("StatusBadge", () => {
  it("renders the status string as its visible label when no label prop is given", () => {
    const wrapper = mount(StatusBadge, { props: { status: "running" } });
    // Template renders {{ label ?? status }} — with no label, the raw status shows.
    expect(wrapper.text()).toContain("running");
  });

  it("maps Running → success (green) and Stopped → info (grey) via data-kind", () => {
    // "running" lowercases to the success kind; "stopped" → info.
    const running = mount(StatusBadge, { props: { status: "Running" } });
    expect(running.attributes("data-kind")).toBe("success");
    // success kind resolves to the green design-token CSS variable.
    expect(running.attributes("style")).toContain("--eu-badge-color: var(--eu-color-success)");

    const stopped = mount(StatusBadge, { props: { status: "Stopped" } });
    expect(stopped.attributes("data-kind")).toBe("info");
    // info kind maps to the secondary text token (grey).
    expect(stopped.attributes("style")).toContain("--eu-badge-color: var(--eu-text-secondary)");
  });

  it("maps Error → danger (red) and exposes the kind on data-kind", () => {
    const errored = mount(StatusBadge, { props: { status: "error" } });
    expect(errored.attributes("data-kind")).toBe("danger");
    expect(errored.attributes("style")).toContain("--eu-badge-color: var(--eu-color-danger)");

    // "failed" is an alias of "error" and must also resolve to danger.
    const failed = mount(StatusBadge, { props: { status: "failed" } });
    expect(failed.attributes("data-kind")).toBe("danger");
  });

  it("uses the custom label prop and overrides the displayed status text", () => {
    // label ?? status → when label is set, it wins over the raw status string.
    const wrapper = mount(StatusBadge, {
      props: { status: "running", label: "运行中" },
    });
    expect(wrapper.text()).toContain("运行中");
    // The raw status must NOT appear in the rendered text once a label is set.
    expect(wrapper.text()).not.toContain("running");
    // data-kind still derives from status, not from the label.
    expect(wrapper.attributes("data-kind")).toBe("success");
  });

  it("falls back to the info (grey) kind for an unrecognized status", () => {
    // An unmapped status is not an error — it defaults to the neutral info kind
    // so the badge still renders a sensible grey state.
    const wrapper = mount(StatusBadge, { props: { status: "UNKNOWN_STATE" } });
    expect(wrapper.attributes("data-kind")).toBe("info");
    expect(wrapper.attributes("style")).toContain("--eu-badge-color: var(--eu-text-secondary)");
    expect(wrapper.text()).toContain("UNKNOWN_STATE");
  });

  it("renders the status-dot indicator element alongside the label text", () => {
    const wrapper = mount(StatusBadge, { props: { status: "running" } });
    // The colored dot (<i.eu-status-dot>) is part of the visual contract.
    expect(wrapper.find(".eu-status-dot").exists()).toBe(true);
    expect(wrapper.classes()).toContain("eu-status-badge");
  });
});

describe("PriceText", () => {
  it("formats amountMinor (fen) into yuan with 2 decimals and thousands separators", () => {
    // 123456 fen → 1234.56 yuan, grouped with a thousands separator.
    const wrapper = mount(PriceText, { props: { amountMinor: 123456 } });
    expect(wrapper.find(".eu-price-amount").text()).toBe("1,234.56");
  });

  it("formats a large minor-unit amount with correct grouping at higher scales", () => {
    // 123456789 fen → 1,234,567.89 yuan — exercises two group separators.
    const wrapper = mount(PriceText, { props: { amountMinor: 123456789 } });
    expect(wrapper.find(".eu-price-amount").text()).toBe("1,234,567.89");
  });

  it("parses amountDecimal string and formats it with 2 decimals + grouping", () => {
    // "9999.5" → 9,999.50 — pads to 2 decimals and adds the group separator.
    const wrapper = mount(PriceText, { props: { amountDecimal: "9999.5" } });
    expect(wrapper.find(".eu-price-amount").text()).toBe("9,999.50");
  });

  it("shows the ¥ symbol when currency is CNY (the default)", () => {
    // currency defaults to "CNY" — symbol computed = "¥".
    const defaultWrapper = mount(PriceText, { props: { amountMinor: 100 } });
    expect(defaultWrapper.find(".eu-price-symbol").text()).toBe("¥");
    expect(defaultWrapper.find(".eu-price-amount").text()).toBe("1.00");

    const explicitCny = mount(PriceText, {
      props: { amountMinor: 100, currency: "CNY" },
    });
    expect(explicitCny.find(".eu-price-symbol").text()).toBe("¥");
  });

  it("switches to the $ symbol for a non-CNY currency", () => {
    // Any currency !== "CNY" resolves to the dollar symbol.
    const wrapper = mount(PriceText, {
      props: { amountMinor: 123456, currency: "USD" },
    });
    expect(wrapper.find(".eu-price-symbol").text()).toBe("$");
    // Amount formatting is currency-agnostic — still 2 decimals + grouping.
    expect(wrapper.find(".eu-price-amount").text()).toBe("1,234.56");
  });

  it("renders 0.00 when neither amountMinor nor amountDecimal is provided", () => {
    // No amount props → formatted returns the fallback "0.00".
    const wrapper = mount(PriceText, { props: {} });
    expect(wrapper.find(".eu-price-amount").text()).toBe("0.00");
    // Default CNY symbol still present.
    expect(wrapper.find(".eu-price-symbol").text()).toBe("¥");
  });
});
