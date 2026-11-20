// SPDX-License-Identifier: Apache-2.0

/** Events of the optional SDK logger. */
export enum LogEvent {
  Rpc = "rpc",
  TransportSelected = "transport_selected",
  RelayRecovery = "relay_recovery",
  RelayGrant = "relay_grant",
  SessionEnded = "session_ended",
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
}

/** The SDK logs nothing unless the caller passes a logger. */
export type Logger = (record: LogRecord) => void;

/** Calls the logger and ignores its failures. */
export function safeLog(logger: Logger | undefined, record: LogRecord): void {
  if (logger === undefined) {
    return;
  }
  try {
    logger(record);
  } catch {
    // A failing logger must not change the result of an operation.
  }
}
