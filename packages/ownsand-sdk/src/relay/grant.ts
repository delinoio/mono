// SPDX-License-Identifier: Apache-2.0
import { durationMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";

import { isInterruption, retryDelayMs } from "../errors.ts";
import { RelayOperationKind } from "../gen/delino/ownsand/v1/common_pb.ts";
import { newUuidV7 } from "../ids.ts";
import { LogEvent } from "../log.ts";
import type { RelayContext } from "./context.ts";
import { backoffMs, RecoveryWindow, RECOVERY_WINDOW_MS } from "./recovery.ts";

/** The control plane's relay stream limit plus 30 seconds. */
export const SERVER_STREAM_NEED_MS = 14 * 60_000 + 30_000;
/** The control plane's `WriteFile` call limit plus 30 seconds. */
export const WRITE_CALL_NEED_MS = 5 * 60_000 + 30_000;
/** The agent may release a grant after this long without an open stream. */
export const GRANT_IDLE_RELEASE_MS = 60_000;

interface HeldGrant {
  id: string;
  /** Monotonic expiry, measured from `expires_in` at receipt. */
  expiresAt: number;
}

/**
 * Holds the relay grant of one operation. It authorizes the operation once,
 * reuses the grant for every stream of the operation and renews it by
 * replacement only when a new stream must start.
 */
export class OperationGrant {
  #grant: HeldGrant | undefined;
  /** The grant that the next authorization replaces, after a discard. */
  #replaces: string | undefined;
  /** Monotonic time since which no stream of the grant has been open. */
  #idleSince = 0;
  #openStreams = 0;

  constructor(
    private readonly ctx: RelayContext,
    private readonly kind: RelayOperationKind,
    private readonly operationId: string,
    private readonly digest: Uint8Array | undefined,
  ) {}

  /** The current grant ID, if any. */
  get grantId(): string | undefined {
    return this.#grant?.id;
  }

  /**
   * Returns a grant that is valid for a new stream of `needMs`. It authorizes
   * or renews only when no stream of the grant is open. Without a recovery
   * window, authorization retries run in their own 60-second window.
   */
  async acquire(
    needMs: number,
    signal: AbortSignal,
    window?: RecoveryWindow,
  ): Promise<string> {
    if (this.#openStreams > 0) {
      throw new Error(
        "relay grant acquired while a stream of the grant is open",
      );
    }
    const now = this.ctx.clock.now();
    const g = this.#grant;
    if (
      g &&
      now + needMs <= g.expiresAt &&
      now - this.#idleSince <= GRANT_IDLE_RELEASE_MS
    ) {
      return g.id;
    }
    const replaces = g?.id ?? this.#replaces;
    const next = await this.#authorize(
      replaces,
      signal,
      window ?? new RecoveryWindow(this.ctx.clock),
    );
    if (replaces !== undefined) {
      this.ctx.log({
        event: LogEvent.RELAY_GRANT_RENEWED,
        rpc: "AuthorizeRelay",
        sandboxId: this.ctx.sandboxId,
      });
    }
    this.#grant = next;
    this.#replaces = undefined;
    this.#idleSince = this.ctx.clock.now();
    return next.id;
  }

  /** Drops the grant after a failure; the next authorization replaces it. */
  discard(): void {
    if (this.#grant) {
      this.#replaces = this.#grant.id;
    }
    this.#grant = undefined;
  }

  streamOpened(): void {
    this.#openStreams++;
  }

  streamClosed(): void {
    this.#openStreams = Math.max(0, this.#openStreams - 1);
    this.#idleSince = this.ctx.clock.now();
  }

  async #authorize(
    replaces: string | undefined,
    signal: AbortSignal,
    window: RecoveryWindow,
  ): Promise<HeldGrant> {
    // One authorization_id per logical request, reused for every retry.
    const authorizationId = newUuidV7(this.ctx.clock.wallNow());
    for (let attempt = 0; ; attempt++) {
      try {
        const res = await this.ctx.authorizeRelay(
          {
            organizationId: this.ctx.organizationId,
            sandboxId: this.ctx.sandboxId,
            operationKind: this.kind,
            operationId: this.operationId,
            requestDigest:
              this.kind === RelayOperationKind.ATTACH_EXEC
                ? new Uint8Array()
                : this.digest,
            authorizationId,
            replacesGrantId: replaces,
          },
          signal,
        );
        const receivedAt = this.ctx.clock.now();
        if (!res.grantId || !res.expiresIn) {
          throw new ConnectError(
            "AuthorizeRelay returned no grant",
            Code.Internal,
          );
        }
        return {
          id: res.grantId,
          expiresAt: receivedAt + durationMs(res.expiresIn),
        };
      } catch (err) {
        if (signal.aborted) {
          throw err;
        }
        const e = ConnectError.from(err);
        const retryable =
          isInterruption(err) ||
          e.code === Code.Unavailable ||
          e.code === Code.ResourceExhausted;
        if (!retryable) {
          throw err;
        }
        let delay = backoffMs(attempt);
        if (e.code === Code.ResourceExhausted) {
          delay = Math.max(delay, retryDelayMs(e) ?? 0);
        }
        if (window.remaining() <= delay) {
          throw err;
        }
        await this.ctx.clock.sleep(delay, signal);
      }
    }
  }
}

export { RECOVERY_WINDOW_MS };
