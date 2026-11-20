// SPDX-License-Identifier: Apache-2.0
import type { Clock } from "../clock.ts";
import { Timing } from "./context.ts";

/** A source of the remaining time of a retry budget. */
export interface RetryBudget {
  remainingMs(): number;
}

/** A fixed budget that starts when it is created. */
export class FixedBudget implements RetryBudget {
  readonly #clock: Clock;
  readonly #deadline: number;

  constructor(clock: Clock, durationMs: number = Timing.recoveryWindowMs) {
    this.#clock = clock;
    this.#deadline = clock.now() + durationMs;
  }

  remainingMs(): number {
    return this.#deadline - this.#clock.now();
  }
}

/**
 * The relay recovery window. A failure starts it; `accepted` pauses its
 * clock; delivered data, a terminal result, a clean rotation of an accepted
 * continuation or a `paused` event ends it; an interrupted continuation
 * resumes the clock of the same window.
 */
export class RecoveryWindow implements RetryBudget {
  readonly #clock: Clock;
  #active = false;
  #consumedMs = 0;
  #runningSince: number | undefined;
  #attempt = 0;

  constructor(clock: Clock) {
    this.#clock = clock;
  }

  get active(): boolean {
    return this.#active;
  }

  /** Records a failure: starts the window or resumes its clock. */
  fail(): void {
    if (!this.#active) {
      this.#active = true;
      this.#consumedMs = 0;
      this.#attempt = 0;
    }
    this.#runningSince ??= this.#clock.now();
  }

  /** Stops the clock while an accepted continuation stays open. */
  pause(): void {
    if (this.#runningSince !== undefined) {
      this.#consumedMs += this.#clock.now() - this.#runningSince;
      this.#runningSince = undefined;
    }
  }

  /** Ends the window. */
  end(): void {
    this.#active = false;
    this.#runningSince = undefined;
    this.#consumedMs = 0;
    this.#attempt = 0;
  }

  remainingMs(): number {
    if (!this.#active) {
      return Number.POSITIVE_INFINITY;
    }
    const running =
      this.#runningSince === undefined
        ? 0
        : this.#clock.now() - this.#runningSince;
    return Timing.recoveryWindowMs - this.#consumedMs - running;
  }

  /** The next back-off delay, never longer than the remaining time. */
  nextBackoffMs(minimumMs = 0): number {
    const delay = Math.min(
      Timing.backoffMaxMs,
      Timing.backoffInitialMs * 2 ** this.#attempt,
    );
    this.#attempt++;
    return Math.max(minimumMs, delay);
  }
}

/** Back-off delay of the n-th retry (0-based) of an authorization request. */
export function backoffMs(attempt: number): number {
  return Math.min(Timing.backoffMaxMs, Timing.backoffInitialMs * 2 ** attempt);
}
