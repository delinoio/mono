// SPDX-License-Identifier: Apache-2.0

/**
 * Time source of the SDK. `now` is a monotonic clock in milliseconds; grant
 * lifetimes, recovery windows and stream idle timers use it, so a wall clock
 * offset does not change them. `sleep` resolves after `ms` milliseconds or
 * rejects with the signal's reason when the signal aborts.
 */
export interface Clock {
  now(): number;
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
  /** Wall clock in Unix milliseconds, used only for UUID v7 timestamps. */
  wallNow(): number;
}

/** The real clock: `performance.now()` and `setTimeout`. */
export const systemClock: Clock = {
  now: () => performance.now(),
  wallNow: () => Date.now(),
  sleep(ms, signal) {
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason);
        return;
      }
      const timer = setTimeout(
        () => {
          signal?.removeEventListener("abort", onAbort);
          resolve();
        },
        Math.max(0, ms),
      );
      const onAbort = () => {
        clearTimeout(timer);
        reject(signal?.reason);
      };
      signal?.addEventListener("abort", onAbort, { once: true });
    });
  },
};
