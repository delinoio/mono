// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";

import type { Clock } from "../clock.ts";
import { isInterruption, isLocalError } from "../errors.ts";

/** Length of one relay recovery window and of the authorization retry window. */
export const RECOVERY_WINDOW_MS = 60_000;
/** A server stream without a response byte for this long is interrupted. */
export const STREAM_WATCHDOG_MS = 90_000;
/** SDK-side deadline of one `WriteFile` attempt. */
export const WRITE_ATTEMPT_DEADLINE_MS = 5 * 60_000 + 30_000;

/**
 * A 60-second budget whose clock can stop. Relay recovery stops it while an
 * accepted continuation stays open and resumes it when that continuation
 * ends without data.
 */
export class RecoveryWindow {
  #used = 0;
  #runningSince: number | undefined;
  attempts = 0;

  constructor(
    private readonly clock: Clock,
    private readonly budgetMs = RECOVERY_WINDOW_MS,
  ) {
    this.#runningSince = clock.now();
  }

  pause(): void {
    if (this.#runningSince !== undefined) {
      this.#used += this.clock.now() - this.#runningSince;
      this.#runningSince = undefined;
    }
  }

  resume(): void {
    if (this.#runningSince === undefined) {
      this.#runningSince = this.clock.now();
    }
  }

  remaining(): number {
    const running =
      this.#runningSince === undefined
        ? 0
        : this.clock.now() - this.#runningSince;
    return this.budgetMs - this.#used - running;
  }
}

/** Exponential back-off with a little jitter: 250 ms doubling up to 5 s. */
export function backoffMs(attempt: number): number {
  const base = Math.min(5_000, 250 * 2 ** Math.min(attempt, 10));
  return Math.round(base * (0.8 + Math.random() * 0.2));
}

/**
 * Reports whether a relay RPC failure is retryable under relay recovery: a
 * transport interruption without a valid terminal status, `unavailable`
 * (session end, `GRANT_INVALID`, no agent relay connection), or
 * `resource_exhausted` (the agent is at its stream or credit capacity).
 */
export function isRetryableRelayFailure(err: unknown): boolean {
  if (isLocalError(err)) {
    return false;
  }
  if (isInterruption(err)) {
    return true;
  }
  const e = ConnectError.from(err);
  return e.code === Code.Unavailable || e.code === Code.ResourceExhausted;
}

/** Links a new AbortController to a parent signal. */
export function linkedController(parent: AbortSignal): {
  controller: AbortController;
  dispose: () => void;
} {
  const controller = new AbortController();
  const onAbort = () => controller.abort(parent.reason);
  if (parent.aborted) {
    controller.abort(parent.reason);
  } else {
    parent.addEventListener("abort", onAbort, { once: true });
  }
  return {
    controller,
    dispose: () => parent.removeEventListener("abort", onAbort),
  };
}
