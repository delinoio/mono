// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { DataPathError, OwnSandError } from "../errors.ts";
import {
  expectTransportInterruption,
  expectUnacceptedStreamEnd,
  expectWindowExhausted,
  totalBackoffMs,
} from "./recovery-assert.ts";

describe("expectWindowExhausted", () => {
  test("sums the back-off delays of the retries", () => {
    expect(totalBackoffMs(1)).toBe(500);
    expect(totalBackoffMs(4)).toBe(500 + 1_000 + 2_000 + 4_000);
    expect(totalBackoffMs(6)).toBe(500 + 1_000 + 2_000 + 4_000 + 5_000 + 5_000);
  });

  test("accepts the exact give-up point", () => {
    // 500 + 1000 + 2000 + 4000 + 5000 * 10 = 57500 after 14 retries, and the
    // next 5000 delay would pass 60000. After 13 retries (52500) it still fits.
    expect(totalBackoffMs(14)).toBe(57_500);
    expect(() => expectWindowExhausted(totalBackoffMs(14), 14)).not.toThrow();
  });

  test("rejects an operation that gave up while a delay still fit", () => {
    expect(() => expectWindowExhausted(totalBackoffMs(3), 3)).toThrow();
    expect(() => expectWindowExhausted(totalBackoffMs(13), 13)).toThrow();
  });

  test("rejects an elapsed time that the retries do not explain", () => {
    expect(() => expectWindowExhausted(totalBackoffMs(14) + 1, 14)).toThrow();
  });

  test("rejects an operation that already passed the window", () => {
    expect(() => expectWindowExhausted(totalBackoffMs(15), 15)).toThrow();
  });
});

describe("expectTransportInterruption", () => {
  test("accepts a Connect error that wraps the fake disconnect", () => {
    const error = ConnectError.from(
      new TypeError("terminated: other side closed"),
    );
    expect(() => expectTransportInterruption(error)).not.toThrow();
  });

  test("rejects the virtual clock's deadlock error", () => {
    const deadlock = new Error("virtual clock: deadlock, no timer is pending");
    expect(() => expectTransportInterruption(deadlock)).toThrow();
  });

  test("rejects another Connect error", () => {
    const error = new ConnectError("gone", Code.Unavailable);
    expect(() => expectTransportInterruption(error)).toThrow();
  });
});

describe("expectUnacceptedStreamEnd", () => {
  class UnacceptedStreamEnd extends OwnSandError {
    override name = "UnacceptedStreamEnd";
  }

  test("accepts the unaccepted stream end", () => {
    const error = new UnacceptedStreamEnd(
      "relay stream ended before the agent accepted it",
    );
    expect(() => expectUnacceptedStreamEnd(error)).not.toThrow();
  });

  test("rejects another SDK error and a plain Error", () => {
    const other = new DataPathError(
      "relay stream ended before the agent accepted it",
      { code: Code.Unavailable },
    );
    expect(() => expectUnacceptedStreamEnd(other)).toThrow();
    expect(() =>
      expectUnacceptedStreamEnd(new Error("virtual clock: deadlock")),
    ).toThrow();
  });
});
