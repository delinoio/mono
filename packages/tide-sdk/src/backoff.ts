// SPDX-License-Identifier: Apache-2.0

/** First reconnect delay of the stream helpers. */
export const MIN_BACKOFF_MS = 1_000;
/** Longest reconnect delay of the stream helpers. */
export const MAX_BACKOFF_MS = 30_000;

/**
 * The reconnect delay after `failures` consecutive failed attempts:
 * 1 second, doubled for each failure, at most 30 seconds.
 */
export function backoffMs(failures: number): number {
  const exponent = Math.min(Math.max(failures, 0), 16);
  return Math.min(MAX_BACKOFF_MS, MIN_BACKOFF_MS * 2 ** exponent);
}

/** Reports whether the error is the abort of the given signal. */
export function abortedBy(signal: AbortSignal | undefined): boolean {
  return signal?.aborted === true;
}
