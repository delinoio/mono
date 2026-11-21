// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import {
  classifyFailure,
  DataPathError,
  FailureClass,
  OwnSandError,
  retryAfterMs,
} from "../errors.ts";
import { RelayOperationKind } from "../gen/delino/ownsand/v1/common_pb.ts";
import { LogEvent, safeLog } from "../logger.ts";
import { type OperationContext, Timing, throwIfAborted } from "./context.ts";
import { backoffMs, FixedBudget, type RetryBudget } from "./recovery.ts";
import { linkedController } from "./stream.ts";

/**
 * The reason that cancels an `AuthorizeRelay` attempt whose response has not
 * arrived when its retry window ends. It never reaches the caller: the grant
 * keeper ends the operation with a `DataPathError` instead.
 */
class AuthorizeDeadlineError extends OwnSandError {
  override name = "AuthorizeDeadlineError";
}

/** The kind of the relay call that a grant must cover. */
export enum RelayCallKind {
  /** `Exec`, `AttachExec` or `ReadFile`: bounded by the relay stream limit. */
  ServerStream = "server_stream",
  /** One `WriteFile` chunk: bounded by the relay call limit. */
  WriteChunk = "write_chunk",
}

interface Grant {
  readonly id: string;
  /** Monotonic expiry, measured from receipt with `expires_in`. */
  readonly expiresAt: number;
  /** True until the first stream of the grant starts. */
  fresh: boolean;
}

/**
 * The relay grant of one operation. It authorizes the operation once, uses
 * the grant only for that operation, renews it with `replaces_grant_id` when
 * a new stream could outlive it or when the last stream ended more than 60
 * seconds ago, and never renews while a stream of the grant is open.
 */
export class GrantKeeper {
  readonly #ctx: OperationContext;
  readonly #kind: RelayOperationKind;
  readonly #operationId: string;
  readonly #digest: Uint8Array | undefined;
  #grant: Grant | undefined;
  /** The last grant ID, kept after a discard for `replaces_grant_id`. */
  #lastGrantId: string | undefined;
  #streamOpen = false;
  #lastStreamEnd: number | undefined;

  constructor(
    ctx: OperationContext,
    kind: RelayOperationKind,
    operationId: string,
    digest: Uint8Array | undefined,
  ) {
    this.#ctx = ctx;
    this.#kind = kind;
    this.#operationId = operationId;
    this.#digest = digest;
  }

  /**
   * Returns a grant ID that is valid for a new call of `callKind`. It
   * authorizes or renews when needed. Outside recovery each authorization
   * gets its own 60-second retry window; in recovery the window's budget
   * applies.
   */
  async acquire(
    callKind: RelayCallKind,
    signal: AbortSignal | undefined,
    budget?: RetryBudget,
  ): Promise<string> {
    if (this.#streamOpen) {
      throw new Error("relay grant: a stream of the grant is still open");
    }
    const grant = this.#grant;
    if (
      grant !== undefined &&
      (grant.fresh || !this.#needsRenewal(grant, callKind))
    ) {
      return grant.id;
    }
    return this.#authorize(signal, budget ?? new FixedBudget(this.#ctx.clock));
  }

  /** Discards the grant in recovery; the next acquire renews it. */
  discard(): void {
    this.#grant = undefined;
  }

  /**
   * Discards the grant after a failed relay call, except when the agent
   * rejected the call at its capacity (`resource_exhausted`): that failure
   * says nothing about the grant, so the retry reuses it. `failure` must be
   * the value that `recoverOrThrow` classifies. `acquire` still renews a kept
   * grant that has expired or has been idle for too long.
   */
  discardFor(failure: unknown): void {
    if (classifyFailure(failure) === FailureClass.ResourceExhausted) {
      return;
    }
    this.discard();
  }

  /** Marks the start of a stream or call under the current grant. */
  streamStarted(): void {
    if (this.#grant !== undefined) {
      this.#grant.fresh = false;
    }
    this.#streamOpen = true;
  }

  /** Marks the end of the stream or call. */
  streamEnded(): void {
    this.#streamOpen = false;
    this.#lastStreamEnd = this.#ctx.clock.now();
  }

  #needsRenewal(grant: Grant, callKind: RelayCallKind): boolean {
    const now = this.#ctx.clock.now();
    const limit =
      callKind === RelayCallKind.ServerStream
        ? Timing.relayStreamLimitMs
        : Timing.relayWriteCallLimitMs;
    if (now + limit + Timing.grantMarginMs >= grant.expiresAt) {
      return true;
    }
    return (
      this.#lastStreamEnd !== undefined &&
      now - this.#lastStreamEnd > Timing.grantIdleMs
    );
  }

  async #authorize(
    signal: AbortSignal | undefined,
    budget: RetryBudget,
  ): Promise<string> {
    const authorizationId = this.#ctx.ids.next();
    const replacesGrantId = this.#lastGrantId;
    let lastError: unknown;
    for (let attempt = 0; ; attempt++) {
      throwIfAborted(signal);
      // The attempt runs only for the time left in its window. A finite
      // bound is enforced with a timer and an abort controller, not with a
      // Connect timeout, so a response that never arrives ends the operation.
      const left = budget.remainingMs();
      if (Number.isFinite(left) && left <= 0) {
        throw lastError === undefined
          ? this.#deadlineError(undefined)
          : toConnectError(lastError);
      }
      const { controller, dispose } = linkedController(signal);
      const stop = new AbortController();
      let deadlineReached = false;
      if (Number.isFinite(left)) {
        this.#ctx.clock.sleep(left, stop.signal).then(
          () => {
            deadlineReached = true;
            controller.abort(new AuthorizeDeadlineError("no response"));
          },
          () => {},
        );
      }
      try {
        const res = await this.#ctx.dataPath.authorizeRelay(
          {
            organizationId: this.#ctx.organizationId,
            sandboxId: this.#ctx.sandboxId,
            operationKind: this.#kind,
            operationId: this.#operationId,
            requestDigest: this.#digest ?? new Uint8Array(0),
            authorizationId,
            replacesGrantId,
          },
          { signal: controller.signal },
        );
        const receivedAt = this.#ctx.clock.now();
        const expiresInMs =
          res.expiresIn === undefined
            ? 0
            : Number(res.expiresIn.seconds) * 1000 +
              Math.floor(res.expiresIn.nanos / 1e6);
        this.#grant = {
          id: res.grantId,
          expiresAt: receivedAt + expiresInMs,
          fresh: true,
        };
        this.#lastGrantId = res.grantId;
        safeLog(this.#ctx.logger, {
          event: LogEvent.RelayGrant,
          sandboxId: this.#ctx.sandboxId,
          reason: replacesGrantId === undefined ? "authorized" : "renewed",
        });
        return res.grantId;
      } catch (error) {
        // Stop the attempt's timer and listener before any back-off sleep.
        stop.abort();
        dispose();
        // The caller's cancellation wins over the window's deadline.
        throwIfAborted(signal);
        if (deadlineReached) {
          // Raised before classification: the abort must not be retried as
          // an interruption or converted to a Connect error.
          throw this.#deadlineError(error);
        }
        const failure = classifyFailure(error);
        if (failure === FailureClass.Terminal) {
          throw error;
        }
        lastError = error;
        const delay = Math.max(
          backoffMs(attempt),
          failure === FailureClass.ResourceExhausted ? retryAfterMs(error) : 0,
        );
        if (budget.remainingMs() <= delay) {
          throw toConnectError(error);
        }
        await this.#ctx.clock.sleep(delay, signal);
      } finally {
        stop.abort();
        dispose();
      }
    }
  }

  #deadlineError(cause: unknown): DataPathError {
    return new DataPathError(
      "AuthorizeRelay got no response within the window",
      {
        code: Code.Unavailable,
        cause,
      },
    );
  }
}

function toConnectError(error: unknown): ConnectError {
  return error instanceof ConnectError ? error : ConnectError.from(error);
}
