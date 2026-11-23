// SPDX-License-Identifier: Apache-2.0

/** The time source of the helpers. Tests replace it. */
export interface Clock {
  /** Wall-clock time in Unix milliseconds. */
  now(): number;
  /**
   * Waits for the given time. It rejects with the abort reason when the
   * signal aborts first.
   */
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
}

export const systemClock: Clock = {
  now: () => Date.now(),
  sleep: (ms, signal) =>
    new Promise<void>((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason);
        return;
      }
      const onAbort = () => {
        clearTimeout(timer);
        reject(signal!.reason);
      };
      const timer = setTimeout(() => {
        signal?.removeEventListener("abort", onAbort);
        resolve();
      }, ms);
      signal?.addEventListener("abort", onAbort, { once: true });
    }),
};
