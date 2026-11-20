// SPDX-License-Identifier: Apache-2.0

/**
 * Time source of the SDK. Every timer of the relay data path (grant
 * lifetimes, recovery windows, back-off, the stream watchdog and the
 * `WriteFile` attempt deadline) goes through this interface, so tests can
 * inject a virtual clock and run hours of relay time in milliseconds.
 */
export interface Clock {
  /** Monotonic time in milliseconds. Only differences are meaningful. */
  now(): number;
  /** Wall-clock time in Unix milliseconds. Used only for UUID v7 timestamps. */
  wallNow(): number;
  /**
   * Resolves after `ms` milliseconds of monotonic time. Rejects with the
   * signal's reason when the signal aborts first.
   */
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
}

/** The clock of the running process. */
export const systemClock: Clock = {
  now: () => performance.now(),
  wallNow: () => Date.now(),
  sleep: (ms, signal) =>
    new Promise<void>((resolve, reject) => {
      if (signal?.aborted) {
        reject(abortReason(signal));
        return;
      }
      const onAbort = () => {
        clearTimeout(timer);
        reject(abortReason(signal));
      };
      const timer = setTimeout(
        () => {
          signal?.removeEventListener("abort", onAbort);
          resolve();
        },
        Math.max(0, ms),
      );
      signal?.addEventListener("abort", onAbort, { once: true });
    }),
};

/** Returns the abort reason of a signal as an Error. */
export function abortReason(signal: AbortSignal | undefined): unknown {
  const reason: unknown = signal?.reason;
  return reason ?? new Error("operation aborted");
}
