// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";

import {
  RetryDetailSchema,
  SessionEndDetailSchema,
  type SessionEndReason,
} from "./gen/delino/ownsand/v1/common_pb.ts";

const REDACTED = "[REDACTED]";

/** Marks an error that the SDK created for a relay transport interruption. */
const INTERRUPTED = Symbol.for("ownsand.sdk.interrupted");

/**
 * Holds the credentials that the client has used so it can remove them from
 * error messages. Values stay in memory only.
 */
export class Redactor {
  readonly #secrets = new Set<string>();

  add(secret: string): void {
    if (secret.length === 0) {
      return;
    }
    this.#secrets.add(secret);
    // Keep only the most recent token values of a token function.
    while (this.#secrets.size > 8) {
      const first = this.#secrets.values().next().value as string;
      this.#secrets.delete(first);
    }
  }

  redact(text: string): string {
    let out = text;
    for (const s of this.#secrets) {
      out = out.split(s).join(REDACTED);
    }
    // Defense in depth for credentials that the redactor never saw.
    return out
      .replace(/Bearer\s+[^\s"',;]+/gi, `Bearer ${REDACTED}`)
      .replace(/\bos[deik]_[A-Za-z0-9_-]+/g, REDACTED);
  }

  /**
   * Returns a ConnectError whose message and metadata contain no credential.
   * The original cause is dropped because it may hold request headers.
   */
  error(reason: unknown): ConnectError {
    const e = ConnectError.from(reason);
    const metadata = new Headers();
    e.metadata.forEach((value, key) => {
      if (key === "authorization" || key === "cookie" || key === "set-cookie") {
        return;
      }
      metadata.set(key, this.redact(value));
    });
    const out = new ConnectError(this.redact(e.rawMessage), e.code, metadata);
    out.details = e.details;
    out.isWireError = e.isWireError;
    if (isLocalError(e)) {
      Object.defineProperty(out, LOCAL, { value: true, enumerable: false });
    } else if (isInterruption(e)) {
      markInterruption(out);
    }
    return out;
  }
}

/** Marks an error as a transport interruption without a valid terminal status. */
export function markInterruption<E extends object>(err: E): E {
  Object.defineProperty(err, INTERRUPTED, { value: true, enumerable: false });
  return err;
}

/**
 * Reports whether an error is a transport interruption without a valid
 * terminal Connect status: the connection failed, the stream ended without
 * its end-of-stream message, or the SDK cancelled a stalled attempt. A status
 * that the server sent (a wire error), including an explicit `aborted`, is
 * terminal and not an interruption.
 */
export function isInterruption(err: unknown): boolean {
  if (typeof err !== "object" || err === null) {
    return false;
  }
  if ((err as Record<symbol, unknown>)[LOCAL] === true) {
    return false;
  }
  for (
    let e: unknown = err, depth = 0;
    typeof e === "object" && e !== null && depth < 8;
    depth++
  ) {
    if ((e as Record<symbol, unknown>)[INTERRUPTED] === true) {
      return true;
    }
    e = (e as { cause?: unknown }).cause;
  }
  if (err instanceof ConnectError) {
    // A client-side Connect error without a server status, for example a
    // stream that ended without its end-of-stream message.
    return !err.isWireError && err.code !== Code.Canceled;
  }
  return false;
}

const LOCAL = Symbol.for("ownsand.sdk.local");

/**
 * Returns a ConnectError that the SDK raises itself before or instead of a
 * request (validation, configuration, origin checks). It is never retried.
 */
export function localError(message: string, code: Code): ConnectError {
  const e = new ConnectError(message, code);
  Object.defineProperty(e, LOCAL, { value: true, enumerable: false });
  return e;
}

/** Reports whether an error is a local SDK error. */
export function isLocalError(err: unknown): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    (err as Record<symbol, unknown>)[LOCAL] === true
  );
}

/** The retry delay of a `resource_exhausted` error, in milliseconds. */
export function retryDelayMs(err: ConnectError): number | undefined {
  for (const d of err.findDetails(RetryDetailSchema)) {
    if (d.retryDelay) {
      return (
        Number(d.retryDelay.seconds) * 1000 +
        Math.floor(d.retryDelay.nanos / 1e6)
      );
    }
  }
  return undefined;
}

/** The session-end reason of an `unavailable` relay error. */
export function sessionEndReason(
  err: ConnectError,
): SessionEndReason | undefined {
  return err.findDetails(SessionEndDetailSchema)[0]?.reason;
}

/**
 * A data path failure: the SDK could not complete an exec, read or write over
 * the relay. A data path failure is never an exit code.
 */
export class DataPathError extends ConnectError {
  /** True when the outcome of the exec is unknown (it may have run). */
  readonly outcomeUnknown: boolean;

  constructor(
    message: string,
    code: Code,
    options: { outcomeUnknown?: boolean; cause?: ConnectError } = {},
  ) {
    super(message, code, options.cause?.metadata, undefined, undefined);
    if (options.cause) {
      this.details = options.cause.details;
    }
    // ConnectError's `instanceof` check accepts a subclass only when its name
    // is "ConnectError", so the name stays; `instanceof DataPathError` uses
    // the prototype chain below.
    this.name = "ConnectError";
    this.outcomeUnknown = options.outcomeUnknown ?? false;
  }

  static override [Symbol.hasInstance](v: unknown): boolean {
    return (
      typeof v === "object" &&
      v !== null &&
      Object.prototype.isPrototypeOf.call(DataPathError.prototype, v)
    );
  }
}
