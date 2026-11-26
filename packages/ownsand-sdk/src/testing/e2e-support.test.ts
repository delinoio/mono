// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, rs, test } from "@rstest/core";
import {
  SandboxState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";
import {
  BillingControlStatus,
  CLEANUP_STEP_TIMEOUT_MS,
  type CleanupStep,
  runCleanup,
  type SnapshotStatus,
  transitionBilling,
  waitForSandboxStartup,
  waitForSnapshotReady,
  withBillingCleanup,
  withCleanup,
} from "./e2e-support.ts";
import { VirtualClock } from "./virtual-clock.ts";

const snap = (state: SnapshotState): SnapshotStatus => ({ state });

describe("waitForSandboxStartup", () => {
  test("returns when the sandbox reaches RUNNING before the deadline", async () => {
    const clock = new VirtualClock();
    const get = rs.fn(async () => ({ state: SandboxState.RUNNING }));
    get.mockResolvedValueOnce({ state: SandboxState.CREATING });

    await clock.run(waitForSandboxStartup({ get }, { clock }));

    expect(get).toHaveBeenCalledTimes(2);
    expect(clock.now()).toBe(200);
  });

  test("aborts a stalled first read and reaches lifecycle cleanup", async () => {
    const clock = new VirtualClock();
    const reply = Promise.withResolvers<{ state: SandboxState }>();
    const get = rs.fn((_signal: AbortSignal) => reply.promise);
    const steps: string[] = [];
    const reported: unknown[] = [];
    const deleteError = new Error("sandbox delete failed");
    const wait = withCleanup(
      () => waitForSandboxStartup({ get }, { clock }),
      [
        () => {
          steps.push("delete");
          throw deleteError;
        },
        () => void steps.push("close"),
      ],
      {
        clock,
        stepTimeoutMs: CLEANUP_STEP_TIMEOUT_MS,
        onCleanupError: (error) => reported.push(error),
      },
    );

    await expect(clock.run(wait)).rejects.toThrow(
      "sandbox startup did not reach RUNNING within 60000 ms: last state unknown",
    );
    expect(clock.now()).toBe(60_000);
    expect(get.mock.calls[0]?.[0].aborted).toBe(true);
    expect(steps).toEqual(["delete", "close"]);
    expect(reported).toEqual([deleteError]);

    reply.resolve({ state: SandboxState.RUNNING });
    await clock.advance(200);
    expect(get).toHaveBeenCalledOnce();
  });

  test.each([60_000, 500])(
    "stops polling at %s ms and reports the last state",
    async (timeoutMs) => {
      const clock = new VirtualClock();
      const get = rs.fn(async () => ({ state: SandboxState.CREATING }));
      const wait = waitForSandboxStartup(
        { get },
        {
          clock,
          timeoutMs,
        },
      );

      await expect(clock.run(wait)).rejects.toThrow(
        `sandbox startup did not reach RUNNING within ${timeoutMs} ms: last state CREATING`,
      );
      expect(clock.now()).toBe(timeoutMs);
      const expectedReads = Math.ceil(timeoutMs / 200);
      expect(get).toHaveBeenCalledTimes(expectedReads);
      await clock.advance(200);
      expect(get).toHaveBeenCalledTimes(expectedReads);
    },
  );

  test.each(["success", "error"])(
    "rejects an RPC %s at the deadline before the timer callback runs",
    async (outcome) => {
      const clock = new VirtualClock();
      const sleep = clock.sleep.bind(clock);
      rs.spyOn(clock, "sleep").mockImplementation((ms, signal) =>
        sleep(ms + 1, signal),
      );
      const get = rs.fn(async (signal: AbortSignal) => {
        await sleep(60_000, signal);
        if (outcome === "error") throw new Error("late RPC error");
        return { state: SandboxState.RUNNING };
      });

      await expect(
        clock.run(waitForSandboxStartup({ get }, { clock })),
      ).rejects.toThrow(
        "sandbox startup did not reach RUNNING within 60000 ms: last state unknown",
      );
      expect(clock.now()).toBe(60_000);
      expect(get.mock.calls[0]?.[0].aborted).toBe(true);
    },
  );

  test.each([SandboxState.PAUSED, SandboxState.DELETED, SandboxState.FAILED])(
    "fails promptly on unexpected stable state %s",
    async (state) => {
      const clock = new VirtualClock();
      const get = rs.fn(async () => ({ state }));

      await expect(
        clock.run(waitForSandboxStartup({ get }, { clock })),
      ).rejects.toThrow(
        `sandbox startup reached ${SandboxState[state]} while waiting for RUNNING`,
      );
      expect(get).toHaveBeenCalledOnce();
      expect(clock.now()).toBe(0);
    },
  );

  test("preserves an RPC error that arrives before the deadline", async () => {
    const clock = new VirtualClock();
    const failure = new Error("GetSandbox failed");
    const get = rs.fn(async () => {
      throw failure;
    });

    await expect(
      clock.run(waitForSandboxStartup({ get }, { clock })),
    ).rejects.toBe(failure);
    expect(get).toHaveBeenCalledOnce();
    expect(clock.now()).toBe(0);
  });
});

describe("waitForSnapshotReady", () => {
  test("returns once the snapshot is READY", async () => {
    const clock = new VirtualClock();
    const states = [
      SnapshotState.CREATING,
      SnapshotState.CREATING,
      SnapshotState.READY,
    ];
    let calls = 0;
    await clock.run(
      waitForSnapshotReady(async () => snap(states[calls++]!), {
        timeoutMs: 60_000,
        pollMs: 200,
        clock,
      }),
    );
    expect(calls).toBe(3);
    expect(clock.now()).toBe(400);
  });

  test("fails at the deadline of a snapshot that stays CREATING", async () => {
    const clock = new VirtualClock();
    let calls = 0;
    const wait = waitForSnapshotReady(
      async () => {
        calls++;
        return snap(SnapshotState.CREATING);
      },
      { timeoutMs: 1_000, pollMs: 200, clock },
    );
    await expect(clock.run(wait)).rejects.toThrow(
      "did not become READY within 1000 ms: last state CREATING",
    );
    expect(calls).toBeGreaterThan(1);
    expect(clock.now()).toBe(1_000);
  });

  test.each([
    { replyAt: 1_000, outcome: "READY" },
    { replyAt: 1_001, outcome: "READY" },
    { replyAt: 1_000, outcome: "RPC error" },
    { replyAt: 1_000, outcome: "FAILED" },
  ])(
    "times out when $outcome arrives at $replyAt ms before the timer callback",
    async ({ replyAt, outcome }) => {
      const clock = new VirtualClock();
      const sleep = clock.sleep.bind(clock);
      rs.spyOn(clock, "sleep").mockImplementationOnce((_, signal) =>
        sleep(replyAt + 1, signal),
      );
      const getSnapshot = rs.fn(async (signal: AbortSignal) => {
        await sleep(replyAt, signal);
        if (outcome === "RPC error") throw new Error("late RPC error");
        return snap(
          outcome === "FAILED" ? SnapshotState.FAILED : SnapshotState.READY,
        );
      });
      const wait = waitForSnapshotReady(getSnapshot, {
        timeoutMs: 1_000,
        pollMs: 200,
        clock,
      });

      await expect(clock.run(wait)).rejects.toThrow(
        "did not become READY within 1000 ms: last state unknown",
      );
      expect(clock.now()).toBe(replyAt);
      expect(getSnapshot.mock.calls[0]?.[0].aborted).toBe(true);
      expect(getSnapshot).toHaveBeenCalledOnce();
    },
  );

  test("reports the last timely state without polling after the deadline", async () => {
    const clock = new VirtualClock();
    const sleep = clock.sleep.bind(clock);
    rs.spyOn(clock, "sleep").mockImplementationOnce((_, signal) =>
      sleep(1_101, signal),
    );
    const getSnapshot = rs.fn(async (signal: AbortSignal) => {
      await sleep(900, signal);
      return snap(SnapshotState.CREATING);
    });
    const wait = waitForSnapshotReady(getSnapshot, {
      timeoutMs: 1_000,
      pollMs: 200,
      clock,
    });

    await expect(clock.run(wait)).rejects.toThrow(
      "did not become READY within 1000 ms: last state CREATING",
    );
    expect(clock.now()).toBe(1_100);
    expect(getSnapshot.mock.calls[0]?.[0].aborted).toBe(true);
    expect(getSnapshot).toHaveBeenCalledOnce();
  });

  test("accepts READY one millisecond before the deadline", async () => {
    const clock = new VirtualClock();
    const wait = waitForSnapshotReady(
      async (signal) => {
        await clock.sleep(999, signal);
        return snap(SnapshotState.READY);
      },
      { timeoutMs: 1_000, pollMs: 200, clock },
    );

    await clock.run(wait);
    expect(clock.now()).toBe(999);
  });

  test("cancels a poll that hangs at the deadline", async () => {
    const clock = new VirtualClock();
    let signalSeen: AbortSignal | undefined;
    const wait = waitForSnapshotReady(
      (signal) => {
        signalSeen = signal;
        // Ignores the signal on purpose: the wait must still end.
        return new Promise<SnapshotStatus>(() => {});
      },
      { timeoutMs: 500, pollMs: 200, clock },
    );
    await expect(clock.run(wait)).rejects.toThrow("did not become READY");
    expect(signalSeen?.aborted).toBe(true);
  });

  test("rejects with the error of a failed poll", async () => {
    const clock = new VirtualClock();
    const failure = new Error("poll failed");
    const wait = waitForSnapshotReady(
      async () => {
        throw failure;
      },
      { timeoutMs: 60_000, pollMs: 200, clock },
    );
    await expect(clock.run(wait)).rejects.toBe(failure);
  });

  test("fails on a FAILED snapshot", async () => {
    const clock = new VirtualClock();
    const wait = waitForSnapshotReady(async () => snap(SnapshotState.FAILED), {
      timeoutMs: 60_000,
      pollMs: 200,
      clock,
    });
    await expect(clock.run(wait)).rejects.toThrow("FAILED");
  });

  test("rejects an already-aborted signal before polling or sleeping", async () => {
    const clock = new VirtualClock();
    const sleep = rs.spyOn(clock, "sleep");
    const caller = new AbortController();
    const reason = new Error("caller gave up");
    caller.abort(reason);
    const getSnapshot = rs.fn(() => Promise.resolve(snap(SnapshotState.READY)));

    await expect(
      waitForSnapshotReady(getSnapshot, {
        timeoutMs: 1_000,
        pollMs: 200,
        clock,
        signal: caller.signal,
      }),
    ).rejects.toBe(reason);
    expect(getSnapshot).not.toHaveBeenCalled();
    expect(sleep).not.toHaveBeenCalled();
  });

  test("stops when the caller aborts", async () => {
    const clock = new VirtualClock();
    const caller = new AbortController();
    const reason = new Error("caller gave up");
    const wait = waitForSnapshotReady(
      async () => snap(SnapshotState.CREATING),
      { timeoutMs: 60_000, pollMs: 200, clock, signal: caller.signal },
    );
    const outcome = expect(wait).rejects.toBe(reason);
    await clock.advance(1_000);
    caller.abort(reason);
    await outcome;
  });
});

describe("runCleanup", () => {
  test("runs every step and collects the errors in order", async () => {
    const clock = new VirtualClock();
    const ran: string[] = [];
    const steps: CleanupStep[] = [
      () => {
        ran.push("a");
        throw new Error("a failed");
      },
      async () => {
        ran.push("b");
      },
      async () => {
        ran.push("c");
        throw new Error("c failed");
      },
    ];
    const errors = await clock.run(
      runCleanup(steps, { stepTimeoutMs: 1_000, clock }),
    );
    expect(ran).toEqual(["a", "b", "c"]);
    expect(errors.map((e) => (e as Error).message)).toEqual([
      "a failed",
      "c failed",
    ]);
  });

  test("abandons a step that hangs and goes on", async () => {
    const clock = new VirtualClock();
    const ran: string[] = [];
    const errors = await clock.run(
      runCleanup(
        [
          () => new Promise<void>(() => {}),
          () => {
            ran.push("next");
          },
        ],
        { stepTimeoutMs: 1_000, clock },
      ),
    );
    expect(ran).toEqual(["next"]);
    expect(errors).toHaveLength(1);
    expect((errors[0] as Error).message).toContain("timed out after 1000 ms");
  });
});

describe("withCleanup", () => {
  test("returns the body result after the steps ran", async () => {
    const clock = new VirtualClock();
    const ran: string[] = [];
    const result = await clock.run(
      withCleanup(async () => 7, [() => void ran.push("x")], {
        stepTimeoutMs: 1_000,
        clock,
      }),
    );
    expect(result).toBe(7);
    expect(ran).toEqual(["x"]);
  });

  test("keeps the body failure and still closes after a rejected delete", async () => {
    const clock = new VirtualClock();
    const ran: string[] = [];
    const reported: unknown[] = [];
    const original = new Error("original failure");
    const wait = withCleanup(
      async () => {
        throw original;
      },
      [
        () => {
          ran.push("deleteSnapshot");
          throw new Error("snapshot delete failed");
        },
        () => {
          ran.push("deleteSandbox");
          throw new Error("sandbox delete failed");
        },
        () => void ran.push("close"),
      ],
      {
        stepTimeoutMs: 1_000,
        clock,
        onCleanupError: (error) => reported.push(error),
      },
    );
    await expect(clock.run(wait)).rejects.toBe(original);
    expect(ran).toEqual(["deleteSnapshot", "deleteSandbox", "close"]);
    expect(reported).toHaveLength(2);
  });

  test("throws the cleanup error when the body passes", async () => {
    const clock = new VirtualClock();
    const ran: string[] = [];
    const wait = withCleanup(
      async () => "ok",
      [
        () => {
          throw new Error("delete failed");
        },
        () => void ran.push("close"),
      ],
      { stepTimeoutMs: 1_000, clock },
    );
    await expect(clock.run(wait)).rejects.toThrow("delete failed");
    expect(ran).toEqual(["close"]);
  });

  test("aggregates several cleanup errors of a passing body", async () => {
    const clock = new VirtualClock();
    const wait = withCleanup(
      async () => "ok",
      [
        () => {
          throw new Error("one");
        },
        () => {
          throw new Error("two");
        },
      ],
      { stepTimeoutMs: 1_000, clock },
    );
    await expect(clock.run(wait)).rejects.toBeInstanceOf(AggregateError);
  });
});

describe("withBillingCleanup", () => {
  const billingUrl = "http://billing.test";

  afterEach(() => {
    rs.restoreAllMocks();
  });

  function fixture(
    restore: () => Promise<Response> = async () => new Response(),
    remove: (
      signal: AbortSignal,
      clock: VirtualClock,
    ) => Promise<void> = async () => {},
  ) {
    const clock = new VirtualClock();
    const steps: string[] = [];
    const reported: unknown[] = [];
    const restoreFetch = rs
      .spyOn(globalThis, "fetch")
      .mockImplementation(() => {
        steps.push("restore");
        return restore();
      });
    const sandbox = {
      delete: rs.fn((signal: AbortSignal) => {
        steps.push("delete");
        return remove(signal, clock);
      }),
      close: () => {
        steps.push("close");
      },
    };
    const run = (body: () => Promise<void>) =>
      clock.run(
        withBillingCleanup(body, billingUrl, sandbox, {
          stepTimeoutMs: CLEANUP_STEP_TIMEOUT_MS,
          clock,
          onCleanupError: (error) => reported.push(error),
        }),
      );
    return {
      clock,
      steps,
      reported,
      restoreFetch,
      sandbox,
      run,
      runTransition: (
        get: (signal: AbortSignal) => Promise<{ state: SandboxState }>,
        status: BillingControlStatus,
      ) => run(() => transitionBilling({ get }, billingUrl, status, { clock })),
    };
  }

  test("reaches billing cleanup when the startup read stalls", async () => {
    const cleanup = fixture();
    const reply = Promise.withResolvers<{ state: SandboxState }>();
    const get = rs.fn((_signal: AbortSignal) => reply.promise);

    await expect(
      cleanup.run(() =>
        waitForSandboxStartup({ get }, { clock: cleanup.clock }),
      ),
    ).rejects.toThrow(
      "sandbox startup did not reach RUNNING within 60000 ms: last state unknown",
    );
    expect(cleanup.clock.now()).toBe(60_000);
    expect(get.mock.calls[0]?.[0].aborted).toBe(true);
    expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
    reply.resolve({ state: SandboxState.RUNNING });
    await cleanup.clock.advance(200);
    expect(get).toHaveBeenCalledOnce();
  });

  describe("transitionBilling", () => {
    test.each([BillingControlStatus.Suspended, BillingControlStatus.Allowed])(
      "aborts a stalled %s POST and reaches cleanup",
      async (status) => {
        const cleanup = fixture();
        const reply = Promise.withResolvers<Response>();
        cleanup.restoreFetch.mockImplementationOnce(() => reply.promise);
        const get = rs.fn(async () => ({ state: SandboxState.RUNNING }));

        await expect(cleanup.runTransition(get, status)).rejects.toThrow(
          `billing control ${status} timed out after 60000 ms`,
        );
        expect(cleanup.clock.now()).toBe(60_000);
        const [url, post] = cleanup.restoreFetch.mock.calls[0]!;
        expect(url).toBe(`${billingUrl}/billing/${status}`);
        expect(post?.signal?.aborted).toBe(true);
        expect(cleanup.restoreFetch.mock.calls[1]?.[1]?.signal).not.toBe(
          post?.signal,
        );
        expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
        expect(get).not.toHaveBeenCalled();

        reply.resolve(new Response());
        await cleanup.clock.advance(200);
        expect(get).not.toHaveBeenCalled();
        expect(cleanup.restoreFetch).toHaveBeenCalledTimes(2);
      },
    );

    test("uses separate cleanup budgets and keeps the POST timeout", async () => {
      const cleanup = fixture(
        () => new Promise<Response>(() => {}),
        () => new Promise<void>(() => {}),
      );
      cleanup.restoreFetch.mockImplementationOnce(
        () => new Promise<Response>(() => {}),
      );
      const get = rs.fn(async () => ({ state: SandboxState.PAUSED }));

      await expect(
        cleanup.runTransition(get, BillingControlStatus.Suspended),
      ).rejects.toThrow("billing control suspended timed out after 60000 ms");
      expect(cleanup.clock.now()).toBe(90_000);
      expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
      expect(cleanup.reported).toEqual([
        new Error("cleanup step timed out after 15000 ms"),
        new Error("cleanup step timed out after 15000 ms"),
      ]);
      expect(get).not.toHaveBeenCalled();
    });

    test("aborts while reading a stalled HTTP error body", async () => {
      const cleanup = fixture();
      const response = new Response(null, { status: 500 });
      const text = rs
        .spyOn(response, "text")
        .mockImplementation(() => new Promise<string>(() => {}));
      cleanup.restoreFetch.mockResolvedValueOnce(response);
      const get = rs.fn(async () => ({ state: SandboxState.PAUSED }));

      await expect(
        cleanup.runTransition(get, BillingControlStatus.Suspended),
      ).rejects.toThrow("billing control suspended timed out after 60000 ms");
      expect(cleanup.clock.now()).toBe(60_000);
      expect(text).toHaveBeenCalledOnce();
      expect(cleanup.restoreFetch.mock.calls[0]?.[1]?.signal?.aborted).toBe(
        true,
      );
      expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
      expect(get).not.toHaveBeenCalled();
    });

    test("preserves a timely HTTP error and its response body", async () => {
      const cleanup = fixture();
      cleanup.restoreFetch.mockResolvedValueOnce(
        new Response("link failed", { status: 500 }),
      );
      const get = rs.fn(async () => ({ state: SandboxState.PAUSED }));

      await expect(
        cleanup.runTransition(get, BillingControlStatus.Suspended),
      ).rejects.toThrow("billing control suspended: 500 link failed");
      expect(cleanup.clock.now()).toBe(0);
      expect(get).not.toHaveBeenCalled();
    });

    test("does not poll after a POST settles at the deadline before the timer fires", async () => {
      const cleanup = fixture();
      const sleep = cleanup.clock.sleep.bind(cleanup.clock);
      rs.spyOn(cleanup.clock, "sleep").mockImplementationOnce((ms, signal) =>
        sleep(ms + 1, signal),
      );
      cleanup.restoreFetch.mockImplementationOnce(async () => {
        await sleep(60_000);
        return new Response();
      });
      const get = rs.fn(async () => ({ state: SandboxState.PAUSED }));

      await expect(
        cleanup.runTransition(get, BillingControlStatus.Suspended),
      ).rejects.toThrow("billing control suspended timed out after 60000 ms");
      expect(cleanup.clock.now()).toBe(60_000);
      expect(cleanup.restoreFetch.mock.calls[0]?.[1]?.signal?.aborted).toBe(
        true,
      );
      expect(get).not.toHaveBeenCalled();
    });

    test.each([
      {
        status: BillingControlStatus.Suspended,
        initial: SandboxState.RUNNING,
        target: SandboxState.PAUSED,
      },
      {
        status: BillingControlStatus.Allowed,
        initial: SandboxState.PAUSED,
        target: SandboxState.RUNNING,
      },
    ])(
      "accepts a timely $status transition after a 40-second POST",
      async ({ status, initial, target }) => {
        const cleanup = fixture();
        cleanup.restoreFetch.mockImplementationOnce(async () => {
          await cleanup.clock.sleep(40_000);
          return new Response();
        });
        const get = rs.fn(async (_signal: AbortSignal) => ({ state: target }));
        get.mockResolvedValueOnce({ state: initial });

        await cleanup.runTransition(get, status);
        expect(cleanup.clock.now()).toBe(40_200);
        expect(get).toHaveBeenCalledTimes(2);
        expect(get.mock.calls[0]?.[0]).toBe(
          cleanup.restoreFetch.mock.calls[0]?.[1]?.signal,
        );
      },
    );

    test.each([
      { state: undefined, last: "unknown" },
      { state: SandboxState.RUNNING, last: "RUNNING" },
    ])(
      "gives polling only 20 seconds after a 40-second POST with last state $last",
      async ({ state, last }) => {
        const cleanup = fixture();
        cleanup.restoreFetch.mockImplementationOnce(async () => {
          await cleanup.clock.sleep(40_000);
          return new Response();
        });
        const reply = Promise.withResolvers<{ state: SandboxState }>();
        const get = rs.fn((_signal: AbortSignal) => reply.promise);
        if (state !== undefined) get.mockResolvedValueOnce({ state });

        await expect(
          cleanup.runTransition(get, BillingControlStatus.Suspended),
        ).rejects.toThrow(
          `sandbox did not reach PAUSED within 60000 ms: last state ${last}`,
        );
        expect(cleanup.clock.now()).toBe(60_000);
        expect(get.mock.calls.at(-1)?.[0].aborted).toBe(true);
        expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
        reply.resolve({ state: SandboxState.PAUSED });
        await cleanup.clock.advance(200);
        expect(get).toHaveBeenCalledTimes(state === undefined ? 1 : 2);
      },
    );
  });

  test("attempts each step once in order when the body passes", async () => {
    const cleanup = fixture();
    await cleanup.run(async () => {});
    expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
    expect(cleanup.restoreFetch).toHaveBeenCalledWith(
      `${billingUrl}/billing/allowed`,
      { method: "POST", signal: expect.any(AbortSignal) },
    );
  });

  test("keeps a failed body's error and reports both cleanup failures in order", async () => {
    const bodyError = new Error("billing body failed");
    const deleteError = new Error("delete failed");
    const cleanup = fixture(
      async () => new Response("restore failed", { status: 500 }),
      async () => {
        throw deleteError;
      },
    );
    await expect(
      cleanup.run(async () => {
        throw bodyError;
      }),
    ).rejects.toBe(bodyError);
    expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
    expect(cleanup.reported).toEqual([
      new Error("billing control allowed: 500 restore failed"),
      deleteError,
    ]);
    expect(cleanup.reported[1]).toBe(deleteError);
  });

  test("aggregates cleanup failures of a passing body in step order", async () => {
    const restoreError = new Error("restore fetch failed");
    const deleteError = new Error("delete failed");
    const cleanup = fixture(
      async () => {
        throw restoreError;
      },
      async () => {
        throw deleteError;
      },
    );
    await expect(cleanup.run(async () => {})).rejects.toMatchObject({
      name: "AggregateError",
      errors: [restoreError, deleteError],
    });
    expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
    expect(cleanup.reported).toEqual([]);
  });

  test.each(["restore", "delete"])(
    "continues cleanup after the %s step times out",
    async (pendingStep) => {
      let deleteStart: { time: number; aborted: boolean } | undefined;
      const cleanup = fixture(
        () =>
          pendingStep === "restore"
            ? new Promise<Response>(() => {})
            : Promise.resolve(new Response()),
        (signal, clock) => {
          deleteStart = { time: clock.now(), aborted: signal.aborted };
          return pendingStep === "delete"
            ? new Promise<void>(() => {})
            : Promise.resolve();
        },
      );

      const outcome = cleanup.run(async () => {});
      await expect(outcome).rejects.toThrow(
        "cleanup step timed out after 15000 ms",
      );
      expect(cleanup.clock.now()).toBe(15_000);
      expect(cleanup.steps).toEqual(["restore", "delete", "close"]);
      const restoreSignal = cleanup.restoreFetch.mock.calls[0]?.[1]?.signal;
      const deleteSignal = cleanup.sandbox.delete.mock.calls[0]?.[0];
      expect(restoreSignal).toBeInstanceOf(AbortSignal);
      expect(deleteSignal).toBeInstanceOf(AbortSignal);
      expect(deleteSignal).not.toBe(restoreSignal);
      expect(deleteStart).toEqual({
        time: pendingStep === "restore" ? 15_000 : 0,
        aborted: false,
      });
      expect(
        (pendingStep === "restore" ? restoreSignal : deleteSignal)?.reason,
      ).toMatchObject({ message: "cleanup step timed out after 15000 ms" });
    },
  );
});
