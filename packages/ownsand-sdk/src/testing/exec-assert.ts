// SPDX-License-Identifier: Apache-2.0
//
// Assertion helper of the end-to-end test (src/e2e.test.ts). It lives apart
// from the test file so that a unit test can cover it without a control plane
// (the end-to-end test is skipped unless the OwnSand end-to-end suite starts
// it).
import { expect } from "@rstest/core";
import { type ExecResult, ExecResultKind } from "../operations/exec.ts";

/**
 * Asserts that an exec ended by a normal exit with code 0: not a failure to
 * start, not a sandbox end, not a nonzero code, not a signal-only end and not
 * a timeout.
 */
export function expectCleanExit(result: ExecResult): void {
  expect(result.kind).toBe(ExecResultKind.Exited);
  if (result.kind !== ExecResultKind.Exited) {
    return;
  }
  expect(result.exitCode).toBe(0n);
  expect(result.signal).toBeUndefined();
  expect(result.timedOut).toBe(false);
}
