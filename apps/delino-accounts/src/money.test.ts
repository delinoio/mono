// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  AmountError,
  formatUsageCost,
  formatUsd,
  parseUsd,
  parseUsdCents,
  toAmountInput,
  usd,
} from "./money.ts";

describe("formatUsd", () => {
  test.each([
    [0n, "$0.00"],
    [1n, "$0.00"],
    [4_999n, "$0.00"],
    [5_000n, "$0.01"],
    [10_000n, "$0.01"],
    [12_345_678n, "$12.35"],
    [12_344_999n, "$12.34"],
    [25_000_000n, "$25.00"],
    [1_234_567_890_000n, "$1,234,567.89"],
    [-1_500_000n, "-$1.50"],
    [-5_000n, "-$0.01"],
    [-4_999n, "$0.00"],
    // Beyond the safe integer range of `number`.
    [9_223_372_036_854_775_807n, "$9,223,372,036,854.78"],
    [-9_223_372_036_854_775_808n, "-$9,223,372,036,854.78"],
  ])("formats %s micro-USD as %s", (micros, text) => {
    expect(formatUsd(micros)).toBe(text);
  });
});

describe("formatUsageCost", () => {
  test.each([
    [0n, "$0.00"],
    [1n, "$0.000001"],
    [123n, "$0.000123"],
    [4_567n, "$0.004567"],
    [5_000n, "$0.005"],
    [9_999n, "$0.009999"],
    [10_000n, "$0.01"],
    [10_001n, "$0.01"],
    [1_254_567n, "$1.25"],
    [-123n, "-$0.000123"],
    [-10_000n, "-$0.01"],
  ])("formats %s micro-USD as %s", (micros, text) => {
    expect(formatUsageCost(micros)).toBe(text);
  });
});

describe("parseUsd", () => {
  test.each([
    ["25", 25_000_000n],
    ["25.00", 25_000_000n],
    ["25.5", 25_500_000n],
    [" $25.50 ", 25_500_000n],
    ["$ 10", 10_000_000n],
    ["1,000.00", 1_000_000_000n],
    ["10000", 10_000_000_000n],
    [".5", 500_000n],
    ["5.", 5_000_000n],
    ["0", 0n],
    ["0.000001", 1n],
    // Larger than Number.MAX_SAFE_INTEGER micro-USD; no precision is lost.
    ["9007199254.740993", 9_007_199_254_740_993n],
  ])("parses %j", (text, micros) => {
    expect(parseUsd(text)).toEqual({ ok: true, micros });
  });

  test.each([
    ["", AmountError.Empty],
    ["   ", AmountError.Empty],
    ["$", AmountError.Empty],
    ["abc", AmountError.Invalid],
    ["-1", AmountError.Invalid],
    ["1e3", AmountError.Invalid],
    ["1,00", AmountError.Invalid],
    ["1.2.3", AmountError.Invalid],
    [".", AmountError.Invalid],
    ["0x10", AmountError.Invalid],
    ["0.0000001", AmountError.TooPrecise],
  ])("rejects %j", (text, error) => {
    expect(parseUsd(text)).toEqual({ ok: false, error });
  });
});

describe("parseUsdCents", () => {
  test("accepts whole cents and rejects smaller units", () => {
    expect(parseUsdCents("25.01")).toEqual({ ok: true, micros: 25_010_000n });
    expect(parseUsdCents("25.010")).toEqual({ ok: true, micros: 25_010_000n });
    expect(parseUsdCents("25.001")).toEqual({
      ok: false,
      error: AmountError.NotWholeCents,
    });
  });
});

describe("toAmountInput", () => {
  test.each([
    [usd(10n), "10.00"],
    [25_500_000n, "25.50"],
    [1n, "0.000001"],
    [0n, "0.00"],
    [-1_500_000n, "-1.50"],
  ])("writes %s micro-USD as %j", (micros, text) => {
    expect(toAmountInput(micros)).toBe(text);
  });

  test("round-trips through parseUsd", () => {
    for (const micros of [0n, 1n, 10_000n, 25_000_000n, 123_456_789n]) {
      expect(parseUsd(toAmountInput(micros))).toEqual({ ok: true, micros });
    }
  });
});
