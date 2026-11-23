// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import type { DataPathSelector } from "../datapath.ts";
import { newBindingSalt, readFileBinding, requestDigest } from "../digest.ts";
import { DataPathError, OwnSandError } from "../errors.ts";
import { RelayOperationKind } from "@delino/ownsand-protos/v1/common_pb";
import {
  type FileReadEvent,
  type ReadFileRequest,
  ReadFileRequestSchema,
} from "@delino/ownsand-protos/v1/data_frame_pb";
import { PeerUnavailableError, SessionEndedError } from "../p2p/session.ts";
import { type OperationContext, throwIfAborted } from "../relay/context.ts";
import { GrantKeeper, RelayCallKind } from "../relay/grant.ts";
import { RecoveryWindow } from "../relay/recovery.ts";
import { watchedRelayStream } from "../relay/stream.ts";
import { isExpiredRead, isNotFound, recoverOrThrow } from "./common.ts";

/** A relay stream that ended with OK before `accepted`: not a rotation. */
class UnacceptedStreamEnd extends OwnSandError {
  override name = "UnacceptedStreamEnd";
}

/**
 * Reads a guest file in chunks. It generates the `read_id` and salt right
 * before the first call, continues every interrupted or rotated stream from
 * the next undelivered offset with the same `read_id`, and never delivers a
 * byte twice. A `read_id` that expired and a path that does not exist are
 * visible `not_found` errors with their own messages.
 */
export async function* runReadFile(
  ctx: OperationContext,
  selector: DataPathSelector,
  path: string,
  startOffset: bigint,
  signal: AbortSignal | undefined,
): AsyncGenerator<Uint8Array, bigint> {
  const readId = ctx.ids.next();
  const salt = newBindingSalt();
  const base = (): ReadFileRequest =>
    create(ReadFileRequestSchema, {
      organizationId: ctx.organizationId,
      sandboxId: ctx.sandboxId,
      readId,
      path,
      bindingSalt: salt,
      offset: next,
    });
  let next = startOffset;
  const grants = new GrantKeeper(
    ctx,
    RelayOperationKind.READ_FILE,
    readId,
    requestDigest(readFileBinding(base())),
  );
  const window = new RecoveryWindow(ctx.clock);

  for (;;) {
    throwIfAborted(signal);
    const session = await selector.select(signal);
    const onRelay = session === undefined;
    let events: AsyncIterable<FileReadEvent | undefined>;
    if (onRelay) {
      const grantId = await grants.acquire(
        RelayCallKind.ServerStream,
        signal,
        window.active ? window : undefined,
      );
      grants.streamStarted();
      const request = base();
      events = (async function* () {
        for await (const response of watchedRelayStream(
          ctx,
          grantId,
          signal,
          (options) => ctx.relay.readFile(request, options),
          {
            window,
            isAccepted: (response) =>
              response.event?.event?.case === "accepted",
          },
          undefined,
          (response) => response.event?.event?.case === "end",
        )) {
          yield response.event;
        }
      })();
    } else {
      events = session.readFile(base(), signal);
    }

    let accepted = false;
    let failure: unknown;
    try {
      for await (const event of events) {
        const e = event?.event;
        switch (e?.case) {
          case "accepted":
            accepted = true;
            window.pause();
            break;
          case "chunk": {
            const { offset, data } = e.value;
            if (offset > next) {
              throw new DataPathError(
                "file chunk skips bytes that were not delivered",
                { code: Code.DataLoss },
              );
            }
            const end = offset + BigInt(data.length);
            if (end > next) {
              const fresh = data.subarray(Number(next - offset));
              next = end;
              window.end();
              yield fresh;
            }
            break;
          }
          case "end":
            if (e.value.sizeBytes !== next) {
              throw new DataPathError(
                "file read ended at a size that differs from the delivered bytes",
                { code: Code.DataLoss },
              );
            }
            window.end();
            if (onRelay) grants.streamEnded();
            return next;
          default:
            // `keepalive` and cases of a newer schema.
            break;
        }
      }
      if (onRelay) grants.streamEnded();
      if (onRelay && accepted) {
        window.end();
        continue;
      }
      failure = new UnacceptedStreamEnd(
        "read stream ended before the agent accepted it",
      );
    } catch (error) {
      if (onRelay) grants.streamEnded();
      if (error instanceof DataPathError) {
        throw error;
      }
      failure = error;
    }

    throwIfAborted(signal);
    if (failure instanceof SessionEndedError) {
      if (session !== undefined) selector.sessionEnded(session);
      continue;
    }
    if (failure instanceof PeerUnavailableError) {
      selector.peerFailed(failure, session);
      window.fail();
      continue;
    }
    if (isExpiredRead(failure)) {
      throw new DataPathError("the file read expired", {
        code: Code.NotFound,
        cause: failure,
      });
    }
    if (isNotFound(failure)) {
      // A missing path: keep the guest's message ("file does not exist") and
      // the original error as the cause.
      throw new DataPathError(failure.rawMessage, {
        code: Code.NotFound,
        cause: failure,
      });
    }
    if (onRelay) grants.discardFor(failure);
    await recoverOrThrow(ctx, window, failure, signal);
  }
}
