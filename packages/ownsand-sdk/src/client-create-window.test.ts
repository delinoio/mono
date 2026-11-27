// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type CreateSandboxResponse,
  CreateSandboxResponseSchema,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { BaseImage, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { describe, expect, rs, test } from "@rstest/core";
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

describe("create attempt settlement", () => {
  const created = create(CreateSandboxResponseSchema, {
    sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
  });

  function heldCreate(retries: boolean) {
    // Shift the monotonic clock separately from timer delivery.
    let skew = 0;
    const h = setup((clock) => ({
      clock: {
        now: () => clock.now() + skew,
        wallNow: () => clock.wallNow(),
        sleep: (ms, signal) => clock.sleep(ms, signal),
      },
    }));
    const first = firstFailure();
    const reply = Promise.withResolvers<CreateSandboxResponse>();
    // Bypass fetch's abort handling so a held RPC can fulfill after abort.
    const rpc = rs
      .spyOn(h.client.sandboxes, "createSandbox")
      .mockImplementation(() => reply.promise);
    if (retries) {
      rpc.mockImplementationOnce(async () => {
        throw first;
      });
    }
    return {
      ...h,
      first,
      reply,
      rpc,
      delayTimerBy: (ms: number) => {
        skew += ms;
      },
    };
  }

  function expectAttempts(
    rpc: ReturnType<typeof heldCreate>["rpc"],
    count: number,
  ) {
    expect(rpc).toHaveBeenCalledTimes(count);
    const request = rpc.mock.calls[0]![0];
    expect(request.idempotencyKey).not.toBe("");
    for (const [retry] of rpc.mock.calls.slice(1)) {
      expect(retry).toBe(request);
    }
  }

  test.each([
    ["initial", false, false],
    ["retry", true, false],
    ["expired retry", true, true],
  ])(
    "returns the caller's abort reason after a successful %s",
    async (_name, retries, expire) => {
      const h = heldCreate(retries);
      const abort = new AbortController();
      const reason = new Error("caller gave up");
      const outcome = expect(
        h.client.createSandbox({ ...validCreate, signal: abort.signal }),
      ).rejects.toBe(reason);

      const attemptCount = retries ? 2 : 1;
      await h.clock.advance(expire ? WINDOW_MS : FIRST_BACKOFF_MS);
      expectAttempts(h.rpc, attemptCount);
      const signal = h.rpc.mock.calls[attemptCount - 1]![1]?.signal;
      expect(signal?.aborted).toBe(expire);

      abort.abort(reason);
      h.reply.resolve(created);
      await outcome;
      expectAttempts(h.rpc, attemptCount);
    },
  );

  test.each([
    ["at expiry with the timer delivered", WINDOW_MS, 0, true],
    ["at expiry with the timer pending", WINDOW_MS - 1, 1, false],
    ["after expiry with the timer pending", WINDOW_MS - 1, 2, false],
  ])(
    "returns the prior failure for a successful retry %s",
    async (_name, elapsed, skew, timerFired) => {
      const h = heldCreate(true);
      const outcome = expect(h.client.createSandbox(validCreate)).rejects.toBe(
        h.first,
      );

      await h.clock.advance(elapsed);
      expectAttempts(h.rpc, 2);
      expect(h.rpc.mock.calls[1]![1]?.signal?.aborted).toBe(timerFired);
      h.delayTimerBy(skew);
      h.reply.resolve(created);

      await outcome;
      expectAttempts(h.rpc, 2);
    },
  );

  test("returns the prior failure when a late retry rejects with a different error", async () => {
    const h = heldCreate(true);
    const outcome = expect(h.client.createSandbox(validCreate)).rejects.toBe(
      h.first,
    );
    await h.clock.advance(WINDOW_MS - 1);
    expectAttempts(h.rpc, 2);
    h.delayTimerBy(1);
    h.reply.reject(new ConnectError("later failure", Code.InvalidArgument));

    await outcome;
    expectAttempts(h.rpc, 2);
  });

  test.each([
    ["a retry just before expiry", true, WINDOW_MS - 1],
    ["an initial call after 60 seconds", false, WINDOW_MS + 1],
  ])("accepts a populated response for %s", async (_name, retries, elapsed) => {
    const h = heldCreate(retries);
    const outcome = h.client.createSandbox(validCreate);
    const attemptCount = retries ? 2 : 1;

    await h.clock.advance(elapsed);
    expectAttempts(h.rpc, attemptCount);
    h.reply.resolve(created);

    expect((await outcome).sandboxId).toBe(SANDBOX);
    expectAttempts(h.rpc, attemptCount);
  });

  test("rejects a timely retry response without a sandbox", async () => {
    const h = heldCreate(true);
    const outcome = expect(h.client.createSandbox(validCreate)).rejects.toThrow(
      "CreateSandbox returned no sandbox",
    );

    await h.clock.advance(WINDOW_MS - 1);
    expectAttempts(h.rpc, 2);
    h.reply.resolve(create(CreateSandboxResponseSchema));

    await outcome;
    expectAttempts(h.rpc, 2);
  });
});
