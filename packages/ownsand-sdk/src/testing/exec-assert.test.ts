// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { type ExecResult, ExecResultKind } from "../operations/exec.ts";
import { expectCleanExit } from "./exec-assert.ts";

const exited = (
  fields: Partial<{
    exitCode: bigint | undefined;
    signal: number | undefined;
    timedOut: boolean;
  }>,
): ExecResult => ({
  kind: ExecResultKind.Exited,
  complete: true,
  exitCode: undefined,
  signal: undefined,
  timedOut: false,
  ...fields,
});

describe("expectCleanExit", () => {
  test("accepts exit code 0 without a signal or a timeout", () => {
    expect(() => expectCleanExit(exited({ exitCode: 0n }))).not.toThrow();
  });

  test("rejects a nonzero exit code", () => {
    expect(() => expectCleanExit(exited({ exitCode: 7n }))).toThrow();
  });

  test("rejects a signal-only end", () => {
    expect(() => expectCleanExit(exited({ signal: 9 }))).toThrow();
  });

  test("rejects exit code 0 with a timeout", () => {
    expect(() =>
      expectCleanExit(exited({ exitCode: 0n, timedOut: true })),
    ).toThrow();
  });
});
