// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { formatDuration } from "./format.ts";

const duration = (seconds: bigint, nanos = 0) => ({ seconds, nanos }) as never;

describe("formatDuration", () => {
  test("shows a positive subsecond duration in milliseconds", () => {
    expect(formatDuration(duration(0n, 500_000_000))).toBe("500 ms");
    expect(formatDuration(duration(0n, 1_000_000))).toBe("1 ms");
    expect(formatDuration(duration(0n, 999_999_999))).toBe("999 ms");
  });

  test("shows a positive duration below one millisecond as <1 ms", () => {
    expect(formatDuration(duration(0n, 1))).toBe("<1 ms");
    expect(formatDuration(duration(0n, 999_999))).toBe("<1 ms");
  });

  test("keeps the other outputs", () => {
    expect(formatDuration(undefined)).toBe("—");
    expect(formatDuration(duration(0n))).toBe("0 s");
    expect(formatDuration(duration(45n))).toBe("45 s");
    expect(formatDuration(duration(5400n))).toBe("1 h 30 min");
    expect(formatDuration(duration(90_000n))).toBe("1 d 1 h");
  });
});
