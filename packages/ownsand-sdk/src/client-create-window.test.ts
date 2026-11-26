// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { BaseImage, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { describe, expect, test } from "@rstest/core";
import type { Clock } from "./clock.ts";
import { Timing } from "./relay/context.ts";
import { SANDBOX, setup } from "./testing/harness.ts";
import type { VirtualClock } from "./testing/virtual-clock.ts";

const HOUR = 3_600_000;
const validCreate = {
  source: { baseImage: BaseImage.LINUX },
  resources: { vcpuCount: 2, memoryMib: 2048, diskGib: 10 },
  idleTtl: HOUR,
  maxTtl: 2 * HOUR,
};

// The first back-off of a create retry sleeps this long (virtual time).
const FIRST_BACKOFF_MS = Timing.backoffInitialMs;
// The window starts with the first failure, at virtual time 0.
const WINDOW_MS = Timing.recoveryWindowMs;

/**
 * A clock whose first back-off sleep wakes `lateByMs` after its time, as a
 * suspended process or a starved event loop does. The monotonic time jumps by
 * that amount when the sleep resolves. The late wake is simulated, not a
 * real timer delay.
 */
function lateWakeClock(clock: VirtualClock, lateByMs: number): Clock {
  let skew = 0;
  let slept = false;
  return {
    now: () => clock.now() + skew,
    wallNow: () => clock.wallNow() + skew,
    async sleep(ms, signal) {
      await clock.sleep(ms, signal);
      if (!slept) {
        slept = true;
        skew += lateByMs;
      }
    },
  };
}

const firstFailure = () => new ConnectError("first failure", Code.Unavailable);
// The client sees the failure after it crossed the fake wire, as a copy.
function expectFirstFailure(error: unknown) {
  expect(error).toBeInstanceOf(ConnectError);
  expect((error as ConnectError).code).toBe(Code.Unavailable);
  expect((error as ConnectError).rawMessage).toBe("first failure");
}

describe("the 60-second create retry window after a late back-off wake", () => {
  const created = {
    sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
  };

  function lateSetup(lateByMs: number) {
    const h = setup((clock) => ({ clock: lateWakeClock(clock, lateByMs) }));
    h.api.unary(SandboxService, "CreateSandbox", (_req, call) => {
      if (call.index === 0) throw firstFailure();
      return created as never;
    });
    return { h };
  }

  // The wake leaves exactly `remaining` milliseconds of the window.
  const lateFor = (remaining: number) =>
    WINDOW_MS - FIRST_BACKOFF_MS - remaining;

  test.each([
    ["exactly at the window end", 0],
    ["after the window end", -1],
  ])("returns the last failure without a call %s", async (_name, remaining) => {
    const { h } = lateSetup(lateFor(remaining));
    const error = await h.clock
      .run(h.client.createSandbox(validCreate))
      .catch((e) => e);
    expectFirstFailure(error);
    expect(h.api.callsOf("CreateSandbox")).toHaveLength(1);
  });

  test("retries with the same key when time remains in the window", async () => {
    const { h } = lateSetup(lateFor(1));
    const sandbox = await h.clock.run(h.client.createSandbox(validCreate));
    expect(sandbox.sandboxId).toBe(SANDBOX);
    const calls = h.api.callsOf("CreateSandbox");
    expect(calls).toHaveLength(2);
    expect((calls[1]!.request as any).idempotencyKey).toBe(
      (calls[0]!.request as any).idempotencyKey,
    );
  });

  test("aborts an in-flight retry at the original deadline", async () => {
    const h = setup((clock) => ({ clock: lateWakeClock(clock, lateFor(5)) }));
    let retrySignal: AbortSignal | undefined;
    h.api.unary(SandboxService, "CreateSandbox", (_req, call) => {
      if (call.index === 0) throw firstFailure();
      retrySignal ??= call.signal;
      return new Promise<never>(() => {});
    });

    let settled = false;
    const outcome = h.client
      .createSandbox(validCreate)
      .catch((error: unknown) => error)
      .then((result) => {
        settled = true;
        return result;
      });

    await h.clock.advance(FIRST_BACKOFF_MS + 4);
    expect(h.api.callsOf("CreateSandbox")).toHaveLength(2);
    expect(retrySignal?.aborted).toBe(false);
    expect(settled).toBe(false);

    await h.clock.advance(1);
    expect(retrySignal?.aborted).toBe(true);
    expect(settled).toBe(true);
    expectFirstFailure(await outcome);
    expect(h.api.callsOf("CreateSandbox")).toHaveLength(2);
  });

  test("a caller abort during the back-off rejects with the caller's reason after one call", async () => {
    const { h } = lateSetup(lateFor(0));
    const abort = new AbortController();
    const reason = new Error("caller gave up");
    const outcome = h.client
      .createSandbox({ ...validCreate, signal: abort.signal })
      .catch((e) => e);
    await h.clock.advance(FIRST_BACKOFF_MS / 5);
    abort.abort(reason);
    await h.clock.advance(WINDOW_MS);
    expect(await outcome).toBe(reason);
    expect(h.api.callsOf("CreateSandbox")).toHaveLength(1);
  });
});
