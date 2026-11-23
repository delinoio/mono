// SPDX-License-Identifier: Apache-2.0

/** Events of the optional SDK logger. */
export enum LogEvent {
  Rpc = "rpc",
  TransportSelected = "transport_selected",
  RelayRecovery = "relay_recovery",
  RelayGrant = "relay_grant",
  SessionEnded = "session_ended",
  InvalidFrame = "invalid_frame",
  StreamOverrun = "stream_overrun",
}

/**
 * One log record. It carries RPC names, sandbox IDs, the transport,
 * durations and Connect codes only, never credentials, exec content or file
 * content.
 */
export interface LogRecord {
  event: LogEvent;
  rpc?: string;
  sandboxId?: string;
  transport?: string;
  durationMs?: number;
  code?: string;
  reason?: string;
  /** Frames and bytes that a slow consumer left queued (`stream_overrun`). */
  queuedFrames?: number;
  queuedBytes?: number;
}

/**
 * The SDK logs nothing unless the caller passes a logger. The SDK never waits
 * for a logger: a promise that the logger returns is not awaited, and its
 * rejection is ignored.
 */
export type Logger = (record: LogRecord) => void;

/**
 * Calls the logger and ignores its failures, whether it throws or returns a
 * promise that rejects. It never waits for the logger.
 */
export function safeLog(logger: Logger | undefined, record: LogRecord): void {
  if (logger === undefined) {
    return;
  }
  try {
    const result: unknown = logger(record);
    if (isThenable(result)) {
      // An async logger fails with a rejected promise, which no caller
      // observes. Attach a handler so that the rejection is not reported as
      // unhandled, and do not wait for the promise.
      result.then(undefined, () => {});
    }
  } catch {
    // A failing logger must not change the result of an operation.
  }
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
  return (
    (typeof value === "object" || typeof value === "function") &&
    value !== null &&
    typeof (value as { then?: unknown }).then === "function"
  );
}
