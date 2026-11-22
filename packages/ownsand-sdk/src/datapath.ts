// SPDX-License-Identifier: Apache-2.0

import { Code } from "@connectrpc/connect";
import {
  DataPathError,
  OwnSandConfigError,
  OwnSandHandleClosedError,
} from "./errors.ts";
import { DataPathTransport } from "@delino/ownsand-protos/v1/common_pb";
import { LogEvent, safeLog } from "./logger.ts";
import {
  PeerSession,
  PeerUnavailableError,
  SessionNotOpenError,
} from "./p2p/session.ts";
import type { PeerConnectionConstructor } from "./p2p/webrtc.ts";
import { type OperationContext, throwIfAborted } from "./relay/context.ts";

/** One shared peer session setup and the callers that wait for it. */
interface SessionSetup {
  /** Aborts the setup when the last waiting caller aborted. */
  readonly controller: AbortController;
  readonly promise: Promise<PeerSession | undefined>;
  /** Callers that still wait for the setup. */
  waiters: number;
}

/**
 * Rejects a forced `PEER_TO_PEER` transport without a WebRTC implementation
 * with an `OwnSandConfigError`. The client runs it before `CreateSandbox`, so
 * a create that can never produce a usable handle does not start a billable
 * sandbox; the selector runs it again when it is built.
 */
export function assertTransportAvailable(
  forced: DataPathTransport | undefined,
  Peer: PeerConnectionConstructor | undefined,
): void {
  if (forced === DataPathTransport.PEER_TO_PEER && Peer === undefined) {
    throw new OwnSandConfigError(
      "PEER_TO_PEER transport needs a WebRTC implementation",
    );
  }
}

/**
 * Selects the data path of one sandbox handle. With WebRTC available and no
 * forced transport, it opens one peer-to-peer session per handle and moves
 * the handle to the relay after an ICE failure or a session that cannot
 * open. An `OpenSession` `failed_precondition` (a sandbox that is not
 * `RUNNING`) uses the relay for that attempt only. A relay-only handle never calls `OpenSession`.
 */
export class DataPathSelector {
  readonly #ctx: OperationContext;
  readonly #Peer: PeerConnectionConstructor | undefined;
  readonly #forced: DataPathTransport | undefined;
  #session: PeerSession | undefined;
  #setup: SessionSetup | undefined;
  #onRelay: boolean;
  #closed = false;

  constructor(
    ctx: OperationContext,
    Peer: PeerConnectionConstructor | undefined,
    forced: DataPathTransport | undefined,
  ) {
    this.#ctx = ctx;
    this.#Peer = Peer;
    this.#forced = forced;
    assertTransportAvailable(forced, Peer);
    this.#onRelay = forced === DataPathTransport.RELAY || Peer === undefined;
  }

  /** The transport that the handle uses now. */
  get transport(): DataPathTransport {
    return this.#onRelay
      ? DataPathTransport.RELAY
      : DataPathTransport.PEER_TO_PEER;
  }

  /**
   * Returns the peer session to use, or undefined for the relay. Concurrent
   * callers share one session setup. The setup has its own abort signal, so
   * the first caller's signal does not decide the outcome for the others: it
   * is cancelled only when every caller that waits for it has aborted. Each
   * caller still ends promptly with its own abort reason. A closed handle
   * rejects with `OwnSandHandleClosedError` before it starts a setup; the
   * relay path keeps serving.
   */
  async select(
    signal: AbortSignal | undefined,
  ): Promise<PeerSession | undefined> {
    if (this.#onRelay) {
      return undefined;
    }
    if (this.#closed) {
      throw new OwnSandHandleClosedError();
    }
    if (this.#session !== undefined && !this.#session.closed) {
      return this.#session;
    }
    throwIfAborted(signal);
    const setup = (this.#setup ??= this.#startSetup());
    return this.#join(setup, signal);
  }

  #startSetup(): SessionSetup {
    const controller = new AbortController();
    const setup: SessionSetup = {
      controller,
      waiters: 0,
      promise: this.#open(controller.signal).finally(() => {
        if (this.#setup === setup) {
          this.#setup = undefined;
        }
      }),
    };
    return setup;
  }

  /** Waits for a shared setup until it settles or the caller aborts. */
  #join(
    setup: SessionSetup,
    signal: AbortSignal | undefined,
  ): Promise<PeerSession | undefined> {
    setup.waiters++;
    if (signal === undefined) {
      // A caller without a signal never aborts, so the setup is never
      // cancelled while it waits.
      return setup.promise;
    }
    return new Promise<PeerSession | undefined>((resolve, reject) => {
      const onAbort = () => {
        if (--setup.waiters === 0) {
          // No caller waits any more: cancel the setup, and let the next
          // caller start a fresh one even while this one winds down.
          if (this.#setup === setup) {
            this.#setup = undefined;
          }
          setup.controller.abort(signal.reason);
        }
        reject(signal.reason);
      };
      signal.addEventListener("abort", onAbort, { once: true });
      setup.promise.then(
        (session) => {
          signal.removeEventListener("abort", onAbort);
          resolve(session);
        },
        (error: unknown) => {
          signal.removeEventListener("abort", onAbort);
          reject(error);
        },
      );
    });
  }

  async #open(signal: AbortSignal): Promise<PeerSession | undefined> {
    try {
      const session = await PeerSession.open(this.#ctx, this.#Peer!, signal);
      if (signal.aborted) {
        // The setup finished at the moment that its last caller aborted.
        session.close();
        throw signal.reason;
      }
      this.#session = session;
      this.#log(DataPathTransport.PEER_TO_PEER);
      return session;
    } catch (error) {
      if (signal.aborted) {
        // Every caller aborted, or the handle closed. The cancelled setup is
        // not a peer failure, so the handle stays on peer-to-peer. A close
        // ends every waiter with the closed error, also when a transport
        // reported the abort as another error such as Canceled.
        throw signal.reason instanceof OwnSandHandleClosedError
          ? signal.reason
          : error;
      }
      if (error instanceof SessionNotOpenError) {
        // The sandbox is not RUNNING (for example PAUSED, where an AttachExec
        // still works on the relay). Use the relay for this attempt only: the
        // handle stays on peer-to-peer, so the next operation calls
        // OpenSession again. A forced PEER_TO_PEER transport returns the
        // failed_precondition error.
        if (this.#forced === DataPathTransport.PEER_TO_PEER) {
          throw error.cause;
        }
        return undefined;
      }
      if (error instanceof PeerUnavailableError) {
        this.peerFailed(error);
        return undefined;
      }
      throw error;
    }
  }

  /** The peer path failed; the handle moves to the relay. */
  peerFailed(cause: unknown): void {
    this.#session?.close();
    this.#session = undefined;
    if (this.#forced === DataPathTransport.PEER_TO_PEER) {
      throw new DataPathError("peer-to-peer data path failed", {
        code: Code.Unavailable,
        cause,
      });
    }
    if (!this.#onRelay) {
      this.#onRelay = true;
      this.#log(DataPathTransport.RELAY);
    }
  }

  /**
   * The agent ended `session`; the next operation opens a new one. A late end
   * of a session that the handle already replaced keeps the replacement.
   */
  sessionEnded(session: PeerSession): void {
    if (this.#session === session) {
      this.#session = undefined;
    }
  }

  /**
   * Closes the handle: it closes the session, if any, and cancels the setup
   * in flight. Every caller that waits for that setup ends with
   * `OwnSandHandleClosedError`, and a session that the setup still opens is
   * closed and never installed. Later peer-to-peer calls of `select` reject
   * with the same error. It is idempotent and also works with no session.
   */
  close(): void {
    this.#closed = true;
    const setup = this.#setup;
    this.#setup = undefined;
    setup?.controller.abort(new OwnSandHandleClosedError());
    this.#session?.close();
    this.#session = undefined;
  }

  #log(transport: DataPathTransport): void {
    safeLog(this.#ctx.logger, {
      event: LogEvent.TransportSelected,
      sandboxId: this.#ctx.sandboxId,
      transport: DataPathTransport[transport],
    });
  }
}
