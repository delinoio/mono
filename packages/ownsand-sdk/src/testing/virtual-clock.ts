// SPDX-License-Identifier: Apache-2.0
import type { Clock } from "../clock.ts";

interface Timer {
  at: number;
  seq: number;
  resolve: () => void;
}

/**
 * A virtual clock for tests. Time moves only when `run` finds every task
 * blocked on a timer; it then jumps to the next timer. Long waits such as
 * 14-minute stream rotations therefore take no real time.
 */
export class VirtualClock implements Clock {
  #now = 0;
  #seq = 0;
  readonly #timers: Timer[] = [];
  readonly #wallBase: number;

  constructor(wallBase = Date.UTC(2026, 10, 20, 12, 0, 0)) {
    this.#wallBase = wallBase;
  }

  now(): number {
    return this.#now;
  }

  wallNow(): number {
    return this.#wallBase + this.#now;
  }

  sleep(ms: number, signal?: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason);
        return;
      }
      const timer: Timer = {
        at: this.#now + Math.max(0, ms),
        seq: this.#seq++,
        resolve: () => {
          signal?.removeEventListener("abort", onAbort);
          resolve();
        },
      };
      const onAbort = () => {
        const i = this.#timers.indexOf(timer);
        if (i >= 0) this.#timers.splice(i, 1);
        reject(signal?.reason);
      };
      signal?.addEventListener("abort", onAbort, { once: true });
      this.#timers.push(timer);
    });
  }

  /** Lets pending promise and I/O callbacks run. */
  async settle(): Promise<void> {
    for (let i = 0; i < 30; i++) {
      await new Promise<void>((resolve) => setImmediate(resolve));
    }
  }

  /** Advances time by `ms`, firing every timer on the way. */
  async advance(ms: number): Promise<void> {
    const target = this.#now + ms;
    for (;;) {
      await this.settle();
      const next = this.#earliest();
      if (next === undefined || next.at > target) {
        break;
      }
      this.#fire(next);
    }
    this.#now = target;
    await this.settle();
  }

  /**
   * Runs until the promise settles, jumping over timers. It fails when the
   * promise stays pending without any timer, or after `limitMs` of virtual
   * time.
   */
  async run<T>(promise: Promise<T>, limitMs = 6 * 60 * 60 * 1000): Promise<T> {
    let settled = false;
    promise.then(
      () => (settled = true),
      () => (settled = true),
    );
    const deadline = this.#now + limitMs;
    for (;;) {
      await this.settle();
      if (settled) {
        return promise;
      }
      const next = this.#earliest();
      if (next === undefined) {
        throw new Error("virtual clock: deadlock, no timer is pending");
      }
      if (next.at > deadline) {
        throw new Error("virtual clock: time limit reached");
      }
      this.#fire(next);
    }
  }

  #earliest(): Timer | undefined {
    let best: Timer | undefined;
    for (const t of this.#timers) {
      if (
        best === undefined ||
        t.at < best.at ||
        (t.at === best.at && t.seq < best.seq)
      ) {
        best = t;
      }
    }
    return best;
  }

  #fire(timer: Timer): void {
    this.#timers.splice(this.#timers.indexOf(timer), 1);
    this.#now = Math.max(this.#now, timer.at);
    timer.resolve();
  }
}
