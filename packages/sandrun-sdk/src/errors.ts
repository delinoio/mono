// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";

/**
 * Base class of the errors that the SDK itself raises. Errors of the server
 * stay `ConnectError` values with their Connect code; the SDK never maps
 * them to another type.
 */
export class SandRunError extends Error {
  override name = "SandRunError";
}

/** The client configuration is incomplete or invalid. */
export class SandRunConfigError extends SandRunError {
  override name = "SandRunConfigError";
}

/** Maximum depth for error chains and aggregate entries. */
const MAX_CAUSE_DEPTH = 8;
const MAX_REDACTION_VISITS = 64;
const DROPPED = Symbol("unchecked error value");

interface RedactionTraversal {
  ancestors: Set<Error>;
  remaining: number;
}

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
    return !(error instanceof SandRunError);
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

/** Reports whether a `SandRunConfigError` is in the `cause` chain of an error. */
function hasConfigErrorCause(error: Error): boolean {
  let current: unknown = error.cause;
  for (let depth = 0; depth < MAX_CAUSE_DEPTH; depth++) {
    if (current instanceof SandRunConfigError) {
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
 * Redacts error messages and strings in causes and aggregate entries.
 * Preserves Connect diagnostics and error classes. Drops links that cannot
 * be checked within the traversal limits.
 */
export function redactError(
  error: unknown,
  secrets: readonly string[],
): unknown {
  const present = secrets.filter((s) => s.length > 0);
  if (present.length === 0) {
    return error;
  }
  return redactChain(error, present, 0, {
    ancestors: new Set(),
    remaining: MAX_REDACTION_VISITS,
  });
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

function redactChain(
  value: unknown,
  secrets: readonly string[],
  depth: number,
  traversal: RedactionTraversal,
): unknown {
  if (depth > MAX_CAUSE_DEPTH || traversal.remaining === 0) {
    return DROPPED;
  }
  traversal.remaining--;
  if (typeof value === "string") {
    return redactText(value, secrets);
  }
  if (!(value instanceof Error)) {
    return value;
  }
  if (traversal.ancestors.has(value)) {
    return DROPPED;
  }
  traversal.ancestors.add(value);
  const originalCause: unknown = value.cause;
  const checkedCause =
    originalCause === undefined
      ? undefined
      : redactChain(originalCause, secrets, depth + 1, traversal);
  const aggregate =
    value instanceof AggregateError
      ? redactAggregateEntries(value, secrets, depth + 1, traversal)
      : undefined;
  traversal.ancestors.delete(value);

  const cause = checkedCause === DROPPED ? undefined : checkedCause;
  const causeChanged = !Object.is(checkedCause, originalCause);
  if (!aggregate && value instanceof ConnectError) {
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
  if (message === value.message && !causeChanged && !aggregate?.changed) {
    return value;
  }
  const prototype = Object.getPrototypeOf(value);
  if (aggregate) {
    const options = cause === undefined ? undefined : { cause };
    const copy = new AggregateError(aggregate.entries, message, options);
    Object.setPrototypeOf(copy, prototype);
    Object.defineProperty(copy, "name", {
      configurable: true,
      writable: true,
      value: value.name,
    });
    try {
      const stack = value.stack;
      if (typeof stack === "string") {
        copy.stack = redactText(stack, secrets);
      }
    } catch {
      // Keep the fresh stack if the source stack cannot be read.
    }
    return copy;
  }
  if (typeof DOMException === "function" && value instanceof DOMException) {
    // DOMException getters require internal slots from the constructor.
    const copy = new DOMException(message, value.name);
    if (prototype !== DOMException.prototype) {
      Object.setPrototypeOf(copy, prototype);
    }
    if (cause !== undefined) {
      Object.defineProperty(copy, "cause", {
        value: cause,
        writable: true,
        enumerable: false,
        configurable: true,
      });
    }
    return copy;
  }
  // Define the name before restoring the prototype, which may have a
  // read-only name or a getter that requires state from the original error.
  const copy = new Error(message, cause === undefined ? undefined : { cause });
  Object.defineProperty(copy, "name", {
    value: value.name,
    writable: true,
    enumerable: false,
    configurable: true,
  });
  Object.setPrototypeOf(copy, prototype);
  return copy;
}

function redactAggregateEntries(
  value: AggregateError,
  secrets: readonly string[],
  depth: number,
  traversal: RedactionTraversal,
): { entries: unknown[]; changed: boolean } {
  let original: readonly unknown[];
  let length: number;
  try {
    const errors: unknown = value.errors;
    if (!Array.isArray(errors)) {
      return { entries: [], changed: true };
    }
    original = errors;
    length = original.length;
  } catch {
    return { entries: [], changed: true };
  }
  if (!Number.isSafeInteger(length) || length < 0) {
    return { entries: [], changed: true };
  }

  const entries: unknown[] = [];
  let changed = false;
  let index = 0;
  if (depth <= MAX_CAUSE_DEPTH) {
    // Read by index so traversal does not visit the unchecked suffix or use
    // an iterator supplied by the source array.
    for (; index < length && traversal.remaining > 0; index++) {
      let entry: unknown;
      try {
        entry = original[index];
      } catch {
        traversal.remaining--;
        changed = true;
        continue;
      }
      const checked = redactChain(entry, secrets, depth, traversal);
      changed ||= !Object.is(checked, entry);
      if (checked !== DROPPED) {
        entries.push(checked);
      }
    }
  }
  return { entries, changed: changed || index < length };
}
