// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { SnapshotState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type CleanupStep,
  runCleanup,
  type SnapshotStatus,
  waitForSnapshotReady,
  withCleanup,
} from "./e2e-support.ts";
import { VirtualClock } from "./virtual-clock.ts";

const snap = (state: SnapshotState): SnapshotStatus => ({ state });

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
    const wait = waitForSnapshotReady(
      async () => {
        throw new Error("poll failed");
      },
      { timeoutMs: 60_000, pollMs: 200, clock },
    );
    await expect(clock.run(wait)).rejects.toThrow("poll failed");
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
