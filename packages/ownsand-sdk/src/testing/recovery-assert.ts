// SPDX-License-Identifier: Apache-2.0
//
// Assertion helpers of the relay recovery tests (src/exec.test.ts and
// src/file.test.ts). A recovery fixture that never recovers must end with the
// operation's own terminal error. The tests run on a virtual clock, whose
// `run` also rejects (with a plain `Error`) when the operation hangs, so a
// bare "the promise rejected" check cannot tell an SDK error from a stuck
// operation. These helpers pin the exact error and the exact point of the
// recovery window at which the operation gave up.
import { ConnectError } from "@connectrpc/connect";
import { expect } from "@rstest/core";
import { OwnSandError } from "../errors.ts";
import { Timing } from "../relay/context.ts";
import { backoffMs } from "../relay/recovery.ts";

/** The message of the TypeError that the fake API raises for a `Disconnect`. */
export const DISCONNECT_MESSAGE = "terminated: other side closed";

/** The message of the failure that a relay stream OK end before `accepted` makes. */
export const UNACCEPTED_STREAM_END_MESSAGE =
  "relay stream ended before the agent accepted it";

/** The total back-off delay of the first `retries` recovery attempts. */
export function totalBackoffMs(retries: number): number {
  let total = 0;
  for (let attempt = 0; attempt < retries; attempt++) {
    total += backoffMs(attempt);
  }
  return total;
}

/**
 * Asserts that an operation gave up because its recovery window had no time
 * for the next back-off delay. `retries` is the number of back-off sleeps that
 * the operation made before the failure that ended it. The fixtures fail
 * without any delay of their own, so the elapsed virtual time is exactly the
 * sum of those delays, it is still inside the window, and the next delay
 * would not have fit.
 */
export function expectWindowExhausted(
  elapsedMs: number,
  retries: number,
): void {
  expect(retries).toBeGreaterThan(0);
  expect(elapsedMs).toBe(totalBackoffMs(retries));
  expect(elapsedMs).toBeLessThan(Timing.recoveryWindowMs);
  expect(elapsedMs + backoffMs(retries)).toBeGreaterThanOrEqual(
    Timing.recoveryWindowMs,
  );
}

/**
 * Asserts the error that a fake `Disconnect` becomes: a Connect error that
 * wraps the fake's transport `TypeError` as its cause. Anything else, for
 * example the virtual clock's deadlock error, fails.
 */
export function expectTransportInterruption(error: unknown): void {
  expect(error).toBeInstanceOf(ConnectError);
  const cause = (error as ConnectError).cause;
  expect(cause).toBeInstanceOf(TypeError);
  expect((cause as TypeError).message).toBe(DISCONNECT_MESSAGE);
}

/**
 * Asserts the error of a relay stream that ended with OK before `accepted`.
 * The error class is private to the operations, so the check uses its name
 * and message.
 */
export function expectUnacceptedStreamEnd(error: unknown): void {
  expect(error).toBeInstanceOf(OwnSandError);
  expect((error as Error).name).toBe("UnacceptedStreamEnd");
  expect((error as Error).message).toBe(UNACCEPTED_STREAM_END_MESSAGE);
}
