// SPDX-License-Identifier: Apache-2.0
//
// Helpers of the end-to-end test (src/e2e.test.ts) that keep it bounded and
// keep its cleanup running. They are separate from the test file so that
// unit tests can cover them without a control plane (the end-to-end test is
// skipped unless the OwnSand end-to-end suite starts it).
import { type Clock, systemClock } from "../clock.ts";
import { SnapshotState } from "@delino/ownsand-protos/v1/common_pb";

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
  const clock = options.clock ?? systemClock;
  const stop = new AbortController();
  const onCallerAbort = () => stop.abort(options.signal?.reason);
  if (options.signal?.aborted) {
    onCallerAbort();
  } else {
    options.signal?.addEventListener("abort", onCallerAbort, { once: true });
  }
  let lastState: SnapshotState | undefined;
  // The deadline timer ends with the wait: `stop.abort()` below rejects its
  // sleep, which the catch ignores.
  clock.sleep(options.timeoutMs, stop.signal).then(
    () =>
      stop.abort(
        new Error(
          `snapshot did not become READY within ${options.timeoutMs} ms: last state ${
            lastState === undefined ? "unknown" : SnapshotState[lastState]
          }`,
        ),
      ),
    () => {},
  );
  const aborted = new Promise<never>((_, reject) => {
    const fail = () => reject(stop.signal.reason);
    if (stop.signal.aborted) fail();
    else stop.signal.addEventListener("abort", fail, { once: true });
  });
  aborted.catch(() => {});
  try {
    for (;;) {
      const snapshot = await Promise.race([getSnapshot(stop.signal), aborted]);
      lastState = snapshot.state;
      if (snapshot.state === SnapshotState.READY) return;
      if (snapshot.state === SnapshotState.FAILED) {
        throw new Error("snapshot reached FAILED");
      }
      await clock.sleep(options.pollMs, stop.signal);
    }
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
