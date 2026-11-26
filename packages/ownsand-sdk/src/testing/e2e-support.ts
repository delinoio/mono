// SPDX-License-Identifier: Apache-2.0
//
// Helpers of the end-to-end test (src/e2e.test.ts) that keep it bounded and
// keep its cleanup running. They are separate from the test file so that
// unit tests can cover them without a control plane (the end-to-end test is
// skipped unless the OwnSand end-to-end suite starts it).
import { type Clock, systemClock } from "../clock.ts";
import {
  SandboxState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";

interface SandboxStateReader {
  get(signal: AbortSignal): Promise<{ state: SandboxState }>;
}

interface SandboxWaitOptions {
  timeoutMs?: number;
  clock?: Clock;
}

/** Waits for a new sandbox to reach RUNNING. */
export function waitForSandboxStartup(
  sandbox: SandboxStateReader,
  options?: SandboxWaitOptions,
): Promise<void> {
  return waitForSandboxState(sandbox, SandboxState.RUNNING, options);
}

/** Applies a billing status and waits for its sandbox state on one deadline. */
export function transitionBilling(
  sandbox: SandboxStateReader,
  billingUrl: string,
  status: BillingControlStatus,
  options?: SandboxWaitOptions,
): Promise<void> {
  const target =
    status === BillingControlStatus.Suspended
      ? SandboxState.PAUSED
      : SandboxState.RUNNING;
  return waitForSandboxState(sandbox, target, options, { billingUrl, status });
}

async function waitForSandboxState(
  sandbox: SandboxStateReader,
  target: SandboxState,
  { timeoutMs = 60_000, clock = systemClock }: SandboxWaitOptions = {},
  billing?: { billingUrl: string; status: BillingControlStatus },
): Promise<void> {
  const deadline = clock.now() + timeoutMs;
  const stop = new AbortController();
  const stage = billing === undefined ? "sandbox startup" : "sandbox";
  let pendingBilling = billing;
  let lastState: SandboxState | undefined;
  const expire = () =>
    stop.abort(
      new Error(
        pendingBilling === undefined
          ? `${stage} did not reach ${SandboxState[target]} within ${timeoutMs} ms: last state ${
              lastState === undefined ? "unknown" : SandboxState[lastState]
            }`
          : `billing control ${pendingBilling.status} timed out after ${timeoutMs} ms`,
      ),
    );
  const checkDeadline = () => {
    // A request or poll delay can settle before the timer callback runs.
    if (clock.now() >= deadline && !stop.signal.aborted) expire();
    stop.signal.throwIfAborted();
  };
  const poll = async () => {
    try {
      if (billing !== undefined) {
        checkDeadline();
        await setBilling(
          billing.billingUrl,
          billing.status,
          stop.signal,
          checkDeadline,
        );
        checkDeadline();
        pendingBilling = undefined;
      }
      for (;;) {
        checkDeadline();
        const { state } = await sandbox.get(stop.signal);
        checkDeadline();
        lastState = state;
        if (state === target) return;
        if (
          billing === undefined &&
          (state === SandboxState.PAUSED ||
            state === SandboxState.DELETED ||
            state === SandboxState.FAILED)
        ) {
          throw new Error(
            `sandbox startup reached ${SandboxState[state]} while waiting for RUNNING`,
          );
        }
        await clock.sleep(200, stop.signal);
      }
    } catch (error) {
      checkDeadline();
      throw error;
    }
  };

  const aborted = new Promise<never>((_, reject) => {
    stop.signal.addEventListener("abort", () => reject(stop.signal.reason), {
      once: true,
    });
  });
  clock
    .sleep(Math.ceil(deadline - clock.now()), stop.signal)
    .then(expire, () => {});
  try {
    // A request that ignores abort must still let cleanup start.
    await Promise.race([aborted, poll()]);
  } finally {
    stop.abort();
  }
}

/** The part of a snapshot message that the poll reads. */
export interface SnapshotStatus {
  state: SnapshotState;
}

export interface WaitForSnapshotReadyOptions {
  /** The poll gives up after this long, measured on `clock`. */
  timeoutMs: number;
  /** The pause between two polls. */
  pollMs: number;
  /** Time source; defaults to the system clock. */
  clock?: Clock;
  /** Cancels the wait; the call rejects with the signal's reason. */
  signal?: AbortSignal;
}

/**
 * Polls until the snapshot is `READY`. The wait ends with an error when the
 * snapshot is `FAILED`, when `timeoutMs` has passed, or when the caller's
 * signal aborts. The signal given to `getSnapshot` aborts at the deadline,
 * so a poll that hangs is cancelled too; a `getSnapshot` that ignores the
 * signal is abandoned at the deadline instead of blocking the caller.
 */
export async function waitForSnapshotReady(
  getSnapshot: (signal: AbortSignal) => Promise<SnapshotStatus>,
  options: WaitForSnapshotReadyOptions,
): Promise<void> {
  options.signal?.throwIfAborted();
  const clock = options.clock ?? systemClock;
  const deadline = clock.now() + options.timeoutMs;
  const stop = new AbortController();
  const onCallerAbort = () => stop.abort(options.signal?.reason);
  options.signal?.addEventListener("abort", onCallerAbort, { once: true });
  let lastState: SnapshotState | undefined;
  const expire = () =>
    stop.abort(
      new Error(
        `snapshot did not become READY within ${options.timeoutMs} ms: last state ${
          lastState === undefined ? "unknown" : SnapshotState[lastState]
        }`,
      ),
    );
  const checkDeadline = () => {
    // The timer callback can run after a poll delay or RPC has settled.
    if (clock.now() >= deadline && !stop.signal.aborted) expire();
    stop.signal.throwIfAborted();
  };
  const poll = async () => {
    for (;;) {
      checkDeadline();
      let snapshot: SnapshotStatus;
      try {
        snapshot = await getSnapshot(stop.signal);
      } catch (error) {
        checkDeadline();
        throw error;
      }
      checkDeadline();
      lastState = snapshot.state;
      if (lastState === SnapshotState.READY) return;
      if (lastState === SnapshotState.FAILED) {
        throw new Error("snapshot reached FAILED");
      }
      await clock.sleep(options.pollMs, stop.signal);
    }
  };

  const aborted = new Promise<never>((_, reject) => {
    stop.signal.addEventListener("abort", () => reject(stop.signal.reason), {
      once: true,
    });
  });
  clock.sleep(options.timeoutMs, stop.signal).then(expire, () => {});
  try {
    // Race the whole poll so that an RPC that ignores abort cannot block cleanup.
    await Promise.race([aborted, poll()]);
  } finally {
    options.signal?.removeEventListener("abort", onCallerAbort);
    stop.abort();
  }
}

/** One cleanup action; the signal aborts when the step times out. */
export type CleanupStep = (signal: AbortSignal) => unknown;

export interface RunCleanupOptions {
  /** Each step is given this long before it is abandoned. */
  stepTimeoutMs: number;
  clock?: Clock;
}

/**
 * Runs every step in order, whatever an earlier step did, and returns the
 * errors of the steps that failed or timed out, in step order. It never
 * throws.
 */
export async function runCleanup(
  steps: readonly CleanupStep[],
  options: RunCleanupOptions,
): Promise<unknown[]> {
  const clock = options.clock ?? systemClock;
  const errors: unknown[] = [];
  for (const step of steps) {
    const stop = new AbortController();
    clock.sleep(options.stepTimeoutMs, stop.signal).then(
      () =>
        stop.abort(
          new Error(`cleanup step timed out after ${options.stepTimeoutMs} ms`),
        ),
      () => {},
    );
    const aborted = new Promise<never>((_, reject) => {
      stop.signal.addEventListener("abort", () => reject(stop.signal.reason), {
        once: true,
      });
    });
    aborted.catch(() => {});
    try {
      await Promise.race([(async () => step(stop.signal))(), aborted]);
    } catch (error) {
      errors.push(error);
    } finally {
      // Ends the timer of a step that finished in time.
      stop.abort();
    }
  }
  return errors;
}

export interface WithCleanupOptions extends RunCleanupOptions {
  /**
   * Called with each cleanup error when the body failed, because the body's
   * error is the one that is rethrown. Cleanup errors of a passing body are
   * thrown instead.
   */
  onCleanupError?: (error: unknown) => void;
}

/**
 * Runs `body`, then every cleanup step, and settles with the body's outcome.
 * The steps run also when the body throws or never reaches them. A body
 * failure is rethrown as it is, and cleanup errors never replace it: they go
 * to `onCleanupError`. When the body passes and a step fails, the call
 * throws that error (an `AggregateError` for several).
 */
export async function withCleanup<T>(
  body: () => Promise<T>,
  steps: readonly CleanupStep[],
  options: WithCleanupOptions,
): Promise<T> {
  let result: T | undefined;
  let failed = false;
  let failure: unknown;
  try {
    result = await body();
  } catch (error) {
    failed = true;
    failure = error;
  }
  const errors = await runCleanup(steps, options);
  if (failed) {
    for (const error of errors) options.onCleanupError?.(error);
    throw failure;
  }
  if (errors.length === 1) throw errors[0];
  if (errors.length > 1) {
    throw new AggregateError(errors, "several cleanup steps failed");
  }
  return result as T;
}

export const CLEANUP_STEP_TIMEOUT_MS = 15_000;

/** Billing statuses match the path segments of the Go harness endpoint. */
export enum BillingControlStatus {
  Suspended = "suspended",
  Allowed = "allowed",
}

/** Applies a billing status to the organization at once. */
async function setBilling(
  billingUrl: string,
  status: BillingControlStatus,
  signal: AbortSignal,
  checkDeadline = () => signal.throwIfAborted(),
): Promise<void> {
  const res = await fetch(`${billingUrl}/billing/${status}`, {
    method: "POST",
    signal,
  });
  checkDeadline();
  if (!res.ok) {
    throw new Error(
      `billing control ${status}: ${res.status} ${await res.text()}`,
    );
  }
}

export function withBillingCleanup<T>(
  body: () => Promise<T>,
  billingUrl: string,
  sandbox: { delete(signal: AbortSignal): Promise<unknown>; close(): void },
  options: WithCleanupOptions,
): Promise<T> {
  return withCleanup(
    body,
    [
      (signal) => setBilling(billingUrl, BillingControlStatus.Allowed, signal),
      (signal) => sandbox.delete(signal),
      () => sandbox.close(),
    ],
    options,
  );
}
