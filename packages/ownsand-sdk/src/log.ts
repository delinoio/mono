// SPDX-License-Identifier: Apache-2.0
import type { Code } from "@connectrpc/connect";

import type { DataPathTransport } from "./gen/delino/ownsand/v1/common_pb.ts";

/** Kinds of SDK log records. */
export enum LogEvent {
  /** One RPC finished. */
  RPC = "rpc",
  /** The data path transport of a sandbox handle was selected. */
  TRANSPORT_SELECTED = "transport_selected",
  /** A relay operation entered recovery after a failure. */
  RELAY_RECOVERY = "relay_recovery",
  /** A relay grant was renewed by replacement. */
  RELAY_GRANT_RENEWED = "relay_grant_renewed",
}

/**
 * One log record. Records hold RPC names, sandbox IDs, the transport,
 * durations and Connect codes, never credentials, exec content or file
 * content.
 */
export interface LogRecord {
  event: LogEvent;
  rpc?: string;
  sandboxId?: string;
  transport?: DataPathTransport;
  durationMs?: number;
  code?: Code;
}

/** Receives SDK log records. The SDK logs nothing without a logger. */
export type Logger = (record: LogRecord) => void;

export const noopLogger: Logger = () => {};
