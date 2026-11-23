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
 * status is the server's answer and ends the helper with that error. An
 * SDK configuration error, also when Connect wraps it, is never retryable.
 */
export function isRetryableStreamError(error: unknown): boolean {
  if (!(error instanceof ConnectError)) {
    // A fetch or body read failure that Connect did not wrap.
    return !(error instanceof TideError);
  }
  if (hasConfigErrorCause(error)) {
    // Connect wraps an interceptor failure, for example a token source that
    // returned no token. A new stream fails in the same way.
    return false;
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

/** Reports whether a `TideConfigError` is in the `cause` chain of an error. */
function hasConfigErrorCause(error: Error): boolean {
  let current: unknown = error.cause;
  for (let depth = 0; depth < MAX_CAUSE_DEPTH; depth++) {
    if (current instanceof TideConfigError) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    current = current.cause;
  }
  return false;
}

/**
 * Replaces every occurrence of each secret in the error message and in the
 * messages of its `cause` chain, and keeps the Connect code, metadata and
 * details of a `ConnectError` and the class of other errors. Connect errors
 * never contain request headers, but this keeps a credential out of an error
 * even when a lower layer echoed it, for example a fetch failure that Connect
 * keeps as the cause.
 */
export function redactError(
  error: unknown,
  secrets: readonly string[],
): unknown {
  const present = secrets.filter((s) => s.length > 0);
  if (present.length === 0) {
    return error;
  }
  return redactChain(error, present, 0);
}

function redactText(text: string, secrets: readonly string[]): string {
  let out = text;
  for (const secret of secrets) {
    if (out.includes(secret)) {
      out = out.split(secret).join("[redacted]");
    }
  }
  return out;
}

/**
 * Returns the value itself when it contains no secret, or a redacted copy.
 * A string cause is redacted; a cause that is neither a string nor an error
 * stays unchanged.
 */
function redactChain(
  value: unknown,
  secrets: readonly string[],
  depth: number,
): unknown {
  if (typeof value === "string") {
    return redactText(value, secrets);
  }
  if (!(value instanceof Error)) {
    return value;
  }
  if (depth > MAX_CAUSE_DEPTH) {
    // Drop a cause that is too deep to check, so no unchecked text stays.
    return undefined;
  }
  const cause =
    value.cause === undefined
      ? undefined
      : redactChain(value.cause, secrets, depth + 1);
  const causeChanged = cause !== value.cause;
  if (value instanceof ConnectError) {
    const raw = redactText(value.rawMessage, secrets);
    if (raw === value.rawMessage && !causeChanged) {
      return value;
    }
    const copy = new ConnectError(
      raw,
      value.code,
      value.metadata,
      undefined,
      cause,
    );
    copy.details = value.details;
    return copy;
  }
  const message = redactText(value.message, secrets);
  if (message === value.message && !causeChanged) {
    return value;
  }
  // The copy keeps the prototype, so `instanceof` checks of the original
  // class (for example `TypeError` or `TideConfigError`) still hold. Its
  // stack starts with the redacted message.
  const copy = new Error(message, cause === undefined ? undefined : { cause });
  Object.setPrototypeOf(copy, Object.getPrototypeOf(value));
  copy.name = value.name;
  return copy;
}
