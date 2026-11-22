// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BillingReason,
  type ErrorDetail,
  ErrorDetailSchema,
} from "@delino/ownsand-protos/v1/common_pb";

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
 * The sandbox handle was closed (`Sandbox.close()` or a successful
 * `Sandbox.delete()`). It is the error of a peer-to-peer operation on a
 * closed handle and of one whose session setup was still running at the close.
 * A relay-only handle keeps serving after a close.
 */
export class OwnSandHandleClosedError extends OwnSandError {
  override name = "OwnSandHandleClosedError";

  constructor() {
    super("sandbox handle closed");
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

/**
 * The consumer of a peer-to-peer stream did not keep up: the frames that the
 * agent sent and the consumer had not taken yet passed the per-stream limit
 * of frames or bytes. It fails only that stream and the session stays open.
 * It is not a `DataPathError`, so `classifyFailure` treats it as a transport
 * interruption and the operation continues on a new stream from the offsets
 * that it delivered.
 */
export class PeerStreamOverrunError extends OwnSandError {
  override name = "PeerStreamOverrunError";
}

/** Returns the OwnSand ErrorDetail of a Connect error, if it has one. */
export function errorDetailOf(error: unknown): ErrorDetail | undefined {
  if (!(error instanceof ConnectError)) {
    return undefined;
  }
  return error.findDetails(ErrorDetailSchema)[0];
}

/** Most `cause` links that `billingReasonOf` follows. */
const MAX_CAUSE_DEPTH = 8;

/**
 * Returns the billing reason of an error: the `billingReason` of the
 * ErrorDetail of a `resource_exhausted` ConnectError, also when an SDK error
 * such as a `DataPathError` wraps it as its `cause`. It returns
 * `BillingReason.UNSPECIFIED` for every other error.
 *
 * Billing enforcement of the control plane rejects a call with such an error
 * when the organization has no linked billing account (`ACCOUNT_REQUIRED`),
 * is blocked for non-payment (`BLOCKED`) or is suspended (`SUSPENDED`). The
 * error is terminal: the SDK never retries it, and the caller must not retry
 * the call until the billing of the organization changes. Its `rawMessage`
 * names the reason and contains the billing link https://accounts.delino.io.
 */
export function billingReasonOf(error: unknown): BillingReason {
  let current: unknown = error;
  for (let depth = 0; depth <= MAX_CAUSE_DEPTH; depth++) {
    if (current instanceof ConnectError) {
      if (current.code !== Code.ResourceExhausted) {
        return BillingReason.UNSPECIFIED;
      }
      return errorDetailOf(current)?.billingReason ?? BillingReason.UNSPECIFIED;
    }
    if (!(current instanceof Error) || current.cause === undefined) {
      return BillingReason.UNSPECIFIED;
    }
    current = current.cause;
  }
  return BillingReason.UNSPECIFIED;
}

/**
 * Reports whether an error is a billing error: a `resource_exhausted` error
 * with a billing reason (see `billingReasonOf`). Every reason other than
 * `UNSPECIFIED` counts, also a value that a newer server adds.
 */
export function isBillingError(error: unknown): boolean {
  return billingReasonOf(error) !== BillingReason.UNSPECIFIED;
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
      // A billing error is terminal everywhere, also under relay recovery
      // and in the createSandbox retry: only a change of the organization's
      // billing can lift it.
      return isBillingError(error)
        ? FailureClass.Terminal
        : FailureClass.ResourceExhausted;
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
