// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type ErrorDetail,
  ErrorDetailSchema,
} from "./gen/delino/ownsand/v1/common_pb.ts";

/** Base class of the errors that the SDK itself raises. */
export class OwnSandError extends Error {
  override name = "OwnSandError";
}

/** The client configuration is incomplete or invalid. */
export class OwnSandConfigError extends OwnSandError {
  override name = "OwnSandConfigError";
}

/** A request failed validation before the SDK sent it. */
export class OwnSandValidationError extends OwnSandError {
  override name = "OwnSandValidationError";
  /** Field path of the request field that failed, for example `idle_ttl`. */
  readonly field: string;

  constructor(field: string, rule: string) {
    super(`${field} ${rule}`);
    this.field = field;
  }
}

/**
 * A data path failure (peer-to-peer or relay). It is never an exit code.
 * When `outcomeUnknown` is true, the SDK cannot tell whether the command ran
 * or how it ended, for example after `AttachExec` returned `not_found`.
 */
export class DataPathError extends OwnSandError {
  override name = "DataPathError";
  readonly code: Code;
  readonly outcomeUnknown: boolean;

  constructor(
    message: string,
    options: { code: Code; outcomeUnknown?: boolean; cause?: unknown },
  ) {
    super(message, { cause: options.cause });
    this.code = options.code;
    this.outcomeUnknown = options.outcomeUnknown ?? false;
  }
}

/**
 * The agent sent a frame on an open stream that the framing rules reject: a
 * kind that is unspecified or of a newer schema, a kind that does not match
 * its payload, or a stream_id rule violation. It ends only the operation of
 * that stream. `classifyFailure` treats it as terminal: a retry would receive
 * the same frame again, so the operation must not recover from it.
 */
export class InvalidFrameError extends DataPathError {
  override name = "InvalidFrameError";

  constructor(message: string, options: { cause?: unknown } = {}) {
    super(message, { code: Code.Internal, cause: options.cause });
  }
}

/** Returns the OwnSand ErrorDetail of a Connect error, if it has one. */
export function errorDetailOf(error: unknown): ErrorDetail | undefined {
  if (!(error instanceof ConnectError)) {
    return undefined;
  }
  return error.findDetails(ErrorDetailSchema)[0];
}

/** How the relay recovery rules treat a failed call. */
export enum FailureClass {
  /** Ends the operation at once with the error. */
  Terminal = "terminal",
  /** A transport interruption without a valid terminal status. */
  Interruption = "interruption",
  /** `unavailable`, for example AGENT_CHANNEL_LOST or GRANT_INVALID. */
  Unavailable = "unavailable",
  /** `resource_exhausted`: at capacity or rate limited; retry after a delay. */
  ResourceExhausted = "resource_exhausted",
}

/**
 * Classifies an error of an `AuthorizeRelay` or relay call under the relay
 * recovery rules of docs/packages-ownsand-sdk-foundation.md.
 *
 * Transport interruptions are errors that carry no terminal status from the
 * server: a network or body read failure (Connect wraps it with the original
 * error as `cause`), a server stream that ended without its end-of-stream
 * message, and an `aborted` status without an OwnSand ErrorDetail. An
 * `aborted` status with an ErrorDetail is an explicit, terminal result (for
 * example from the guest helper for a file that changed during a read).
 */
export function classifyFailure(error: unknown): FailureClass {
  // Every other DataPathError is an interruption (the write path relies on
  // that for its deadline retry), so this terminal subclass is checked first.
  if (error instanceof InvalidFrameError) {
    return FailureClass.Terminal;
  }
  if (!(error instanceof ConnectError)) {
    return FailureClass.Interruption;
  }
  if (error.cause !== undefined && !(error.cause instanceof ConnectError)) {
    return FailureClass.Interruption;
  }
  if (
    error.rawMessage.startsWith("protocol error: missing EndStreamResponse")
  ) {
    return FailureClass.Interruption;
  }
  switch (error.code) {
    case Code.Unavailable:
      return FailureClass.Unavailable;
    case Code.ResourceExhausted:
      return FailureClass.ResourceExhausted;
    case Code.Aborted:
      return errorDetailOf(error) === undefined
        ? FailureClass.Interruption
        : FailureClass.Terminal;
    default:
      return FailureClass.Terminal;
  }
}

/** Returns the retry delay of an error detail in milliseconds, or 0. */
export function retryAfterMs(error: unknown): number {
  const after = errorDetailOf(error)?.retryAfter;
  if (after === undefined) {
    return 0;
  }
  return Number(after.seconds) * 1000 + Math.floor(after.nanos / 1e6);
}

/** Converts an ErrorDetail of a data channel error frame to a ConnectError. */
export function connectErrorFromDetail(detail: ErrorDetail): ConnectError {
  const code =
    detail.code >= 1 && detail.code <= 16 ? (detail.code as number) : 2;
  return new ConnectError(detail.message, code as Code, undefined, [
    { desc: ErrorDetailSchema, value: detail },
  ]);
}

/**
 * Replaces every occurrence of each secret in the error message. Connect
 * errors never contain request headers, but this keeps a credential out of
 * an error even when a lower layer echoed it.
 */
export function redactError(
  error: unknown,
  secrets: readonly string[],
): unknown {
  const present = secrets.filter((s) => s.length > 0);
  if (present.length === 0 || !(error instanceof Error)) {
    return error;
  }
  let changed = false;
  const redact = (text: string): string => {
    let out = text;
    for (const secret of present) {
      if (out.includes(secret)) {
        out = out.split(secret).join("[redacted]");
        changed = true;
      }
    }
    return out;
  };
  if (error instanceof ConnectError) {
    const raw = redact(error.rawMessage);
    if (!changed) {
      return error;
    }
    const copy = new ConnectError(raw, error.code, error.metadata);
    copy.details = error.details;
    return copy;
  }
  const message = redact(error.message);
  if (!changed) {
    return error;
  }
  const copy = new OwnSandError(message);
  copy.name = error.name;
  return copy;
}
