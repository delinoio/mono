// SPDX-License-Identifier: Apache-2.0
import { ConnectError } from "@connectrpc/connect";
import { classifyFailure, FailureClass, retryAfterMs } from "../errors.ts";
import { RelayOperationKind } from "../gen/delino/ownsand/v1/common_pb.ts";
import { LogEvent, safeLog } from "../logger.ts";
import { type OperationContext, Timing, throwIfAborted } from "./context.ts";
import { backoffMs, FixedBudget, type RetryBudget } from "./recovery.ts";

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
    for (let attempt = 0; ; attempt++) {
      throwIfAborted(signal);
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
          { signal },
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
        throwIfAborted(signal);
        const failure = classifyFailure(error);
        if (failure === FailureClass.Terminal) {
          throw error;
        }
        const delay = Math.max(
          backoffMs(attempt),
          failure === FailureClass.ResourceExhausted ? retryAfterMs(error) : 0,
        );
        if (budget.remainingMs() <= delay) {
          throw error instanceof ConnectError
            ? error
            : ConnectError.from(error);
        }
        await this.#ctx.clock.sleep(delay, signal);
      }
    }
  }
}
