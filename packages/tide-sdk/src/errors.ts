// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";

/**
 * Base class of the errors that the SDK itself raises. Errors of the server
 * stay `ConnectError` values with their Connect code; the SDK never maps
 * them to another type.
 */
export class TideError extends Error {
  override name = "TideError";
}

/** The client configuration is incomplete or invalid. */
export class TideConfigError extends TideError {
  override name = "TideConfigError";
}

/** Most `cause` links that `connectCodeOf` follows. */
const MAX_CAUSE_DEPTH = 8;

/**
 * Returns the Connect code of an error: the code of a `ConnectError`, also
 * when another error wraps it as its `cause`. It returns undefined for an
 * error without a Connect code.
 */
export function connectCodeOf(error: unknown): Code | undefined {
  let current: unknown = error;
  for (let depth = 0; depth <= MAX_CAUSE_DEPTH; depth++) {
    if (current instanceof ConnectError) {
      return current.code;
    }
    if (!(current instanceof Error) || current.cause === undefined) {
      return undefined;
    }
    current = current.cause;
  }
  return undefined;
}

/**
 * Reports whether a failed stream call can continue on a new stream: a
 * transport interruption without a status from the server (a network or
 * body read failure, or a stream that ended without its end-of-stream
 * message), or a status that means "try again later" (`unavailable`,
 * `resource_exhausted`, `aborted` and `deadline_exceeded`). Every other
 * status is the server's answer and ends the helper with that error.
 */
export function isRetryableStreamError(error: unknown): boolean {
  if (!(error instanceof ConnectError)) {
    // A fetch or body read failure that Connect did not wrap.
    return !(error instanceof TideError);
  }
  if (error.cause !== undefined && !(error.cause instanceof ConnectError)) {
    return true;
  }
  if (
    error.rawMessage.startsWith("protocol error: missing EndStreamResponse")
  ) {
    return true;
  }
  switch (error.code) {
    case Code.Unavailable:
    case Code.ResourceExhausted:
    case Code.Aborted:
    case Code.DeadlineExceeded:
      return true;
    default:
      return false;
  }
}

/**
 * Replaces every occurrence of each secret in the error message, and keeps
 * the Connect code, metadata and details of a `ConnectError`. Connect errors
 * never contain request headers, but this keeps a credential out of an error
 * even when a lower layer echoed it.
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
    const copy = new ConnectError(
      raw,
      error.code,
      error.metadata,
      undefined,
      error.cause,
    );
    copy.details = error.details;
    return copy;
  }
  const message = redact(error.message);
  if (!changed) {
    return error;
  }
  const copy = new TideError(message, { cause: error.cause });
  copy.name = error.name;
  return copy;
}
