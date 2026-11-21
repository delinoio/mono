// SPDX-License-Identifier: Apache-2.0

import { Code } from "@connectrpc/connect";
import { DataPathError, OwnSandConfigError } from "./errors.ts";
import { DataPathTransport } from "./gen/delino/ownsand/v1/common_pb.ts";
import { LogEvent, safeLog } from "./logger.ts";
import {
  PeerSession,
  PeerUnavailableError,
  SessionNotOpenError,
} from "./p2p/session.ts";
import type { PeerConnectionConstructor } from "./p2p/webrtc.ts";
import type { OperationContext } from "./relay/context.ts";

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
  #opening: Promise<PeerSession | undefined> | undefined;
  #onRelay: boolean;

  constructor(
    ctx: OperationContext,
    Peer: PeerConnectionConstructor | undefined,
    forced: DataPathTransport | undefined,
  ) {
    this.#ctx = ctx;
    this.#Peer = Peer;
    this.#forced = forced;
    if (forced === DataPathTransport.PEER_TO_PEER && Peer === undefined) {
      throw new OwnSandConfigError(
        "PEER_TO_PEER transport needs a WebRTC implementation",
      );
    }
    this.#onRelay = forced === DataPathTransport.RELAY || Peer === undefined;
  }

  /** The transport that the handle uses now. */
  get transport(): DataPathTransport {
    return this.#onRelay
      ? DataPathTransport.RELAY
      : DataPathTransport.PEER_TO_PEER;
  }

  /** Returns the peer session to use, or undefined for the relay. */
  async select(
    signal: AbortSignal | undefined,
  ): Promise<PeerSession | undefined> {
    if (this.#onRelay) {
      return undefined;
    }
    if (this.#session !== undefined && !this.#session.closed) {
      return this.#session;
    }
    this.#opening ??= this.#open(signal).finally(() => {
      this.#opening = undefined;
    });
    return this.#opening;
  }

  async #open(
    signal: AbortSignal | undefined,
  ): Promise<PeerSession | undefined> {
    try {
      this.#session = await PeerSession.open(this.#ctx, this.#Peer!, signal);
      this.#log(DataPathTransport.PEER_TO_PEER);
      return this.#session;
    } catch (error) {
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

  /** The agent ended the session; the next operation opens a new one. */
  sessionEnded(): void {
    this.#session = undefined;
  }

  /** Closes the session of the handle, if any. */
  close(): void {
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
