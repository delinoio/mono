// SPDX-License-Identifier: Apache-2.0
import type { Clock } from "../../src/clock.ts";

interface Timer {
  at: number;
  seq: number;
  fire: () => void;
}

/** Lets every pending promise chain and I/O callback run. */
async function settle(rounds = 30): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await new Promise<void>((resolve) => setImmediate(resolve));
  }
}

/**
 * A virtual clock. `run()` drives a promise to completion and advances time
 * to the next timer only when nothing else can run, so hours of relay time
 * pass in milliseconds of real time. Tests that use it must not do real I/O.
 */
export class VirtualClock implements Clock {
  #now = 0;
  #seq = 0;
  #timers: Timer[] = [];
  /** Offset of the caller's wall clock from the fake server's wall clock. */
  wallSkewMs = 0;
  readonly wallBase = Date.UTC(2026, 10, 20, 0, 0, 0);

  now(): number {
    return this.#now;
  }

  wallNow(): number {
    return this.wallBase + this.#now + this.wallSkewMs;
  }

  /** The server's wall clock, without the caller skew. */
  serverWallNow(): number {
    return this.wallBase + this.#now;
  }

  sleep(ms: number, signal?: AbortSignal): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason ?? new Error("aborted"));
        return;
      }
      const timer: Timer = {
        at: this.#now + Math.max(0, ms),
        seq: this.#seq++,
        fire: () => {
          signal?.removeEventListener("abort", onAbort);
          resolve();
        },
      };
      const onAbort = () => {
        this.#timers = this.#timers.filter((t) => t !== timer);
        reject(signal?.reason ?? new Error("aborted"));
      };
      signal?.addEventListener("abort", onAbort, { once: true });
      this.#timers.push(timer);
    });
  }

  /** Drives `promise` to completion, advancing virtual time when idle. */
  async run<T>(promise: Promise<T>, limitMs = 48 * 60 * 60 * 1000): Promise<T> {
    let settled = false;
    promise.then(
      () => (settled = true),
      () => (settled = true),
    );
    for (;;) {
      await settle();
      if (settled) {
        return promise;
      }
      if (this.#timers.length === 0) {
        throw new Error(
          `virtual clock deadlock at ${this.#now} ms: the promise is pending and no timer is set`,
        );
      }
      this.#timers.sort((a, b) => a.at - b.at || a.seq - b.seq);
      const t = this.#timers.shift()!;
      this.#now = Math.max(this.#now, t.at);
      if (this.#now > limitMs) {
        throw new Error(`virtual clock passed its limit of ${limitMs} ms`);
      }
      t.fire();
    }
  }
}
