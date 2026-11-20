// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";

import { readBinding, bindingDigest } from "../digest.ts";
import { DataPathError, isInterruption } from "../errors.ts";
import { RelayOperationKind } from "../gen/delino/ownsand/v1/common_pb.ts";
import { ReadFileRequestSchema } from "../gen/delino/ownsand/v1/datapath_pb.ts";
import { newBindingSalt, newUuidV7 } from "../ids.ts";
import { LogEvent } from "../log.ts";
import type { RelayContext } from "./context.ts";
import { OperationGrant, SERVER_STREAM_NEED_MS } from "./grant.ts";
import {
  backoffMs,
  isRetryableRelayFailure,
  RecoveryWindow,
} from "./recovery.ts";
import { watchedStream } from "./stream.ts";

/**
 * Reads one guest file over the relay and yields its contents in chunks. The
 * read keeps one `read_id`, path and salt, and continues from the next
 * undelivered offset after a rotation or a failure.
 */
export async function* relayReadFile(
  ctx: RelayContext,
  path: string,
  signal: AbortSignal,
): AsyncGenerator<Uint8Array, bigint, undefined> {
  // The read_id is generated right before the first call.
  const readId = newUuidV7(ctx.clock.wallNow());
  const salt = newBindingSalt();
  const digest = bindingDigest(readBinding(salt, ctx.sandboxId, readId, path));
  const grant = new OperationGrant(
    ctx,
    RelayOperationKind.READ_FILE,
    readId,
    digest,
  );
  let offset = 0n;
  let window: RecoveryWindow | undefined;
  let lastError: unknown;

  for (;;) {
    signal.throwIfAborted();
    const grantId = await grant.acquire(SERVER_STREAM_NEED_MS, signal, window);
    let accepted = false;
    try {
      const request = create(ReadFileRequestSchema, {
        organizationId: ctx.organizationId,
        sandboxId: ctx.sandboxId,
        readId,
        path,
        bindingSalt: salt,
        offset,
      });
      for await (const res of watchedStream(ctx, grant, signal, (call) =>
        ctx.readFile(grantId, request, call),
      )) {
        const m = res.message;
        switch (m.case) {
          case "accepted":
            accepted = true;
            window?.pause();
            break;
          case "keepalive":
            break;
          case "chunk": {
            const start = m.value.offset;
            const end = start + BigInt(m.value.data.byteLength);
            if (start > offset) {
              throw new DataPathError(
                "relay ReadFile skipped bytes of the file",
                Code.DataLoss,
              );
            }
            if (end <= offset) {
              break;
            }
            const data =
              start < offset
                ? m.value.data.subarray(Number(offset - start))
                : m.value.data;
            // The resume offset advances only after a complete chunk.
            offset = end;
            window = undefined;
            yield data;
            break;
          }
          case "finished":
            if (m.value.sizeBytes !== offset) {
              throw new DataPathError(
                "relay ReadFile ended at a different size than it delivered",
                Code.DataLoss,
              );
            }
            return offset;
          default:
            break;
        }
      }
      if (accepted) {
        window = undefined;
        continue;
      }
      lastError = new DataPathError(
        "relay stream ended before the agent accepted it",
        Code.Unavailable,
      );
    } catch (err) {
      if (signal.aborted || err instanceof DataPathError) {
        throw err;
      }
      const e = ConnectError.from(err);
      if (e.code === Code.NotFound && !isInterruption(err)) {
        // The read_id expired: a visible error.
        throw new DataPathError(
          "the file read expired on the agent",
          Code.NotFound,
          { cause: e },
        );
      }
      if (!isRetryableRelayFailure(err)) {
        throw err;
      }
      lastError = err;
    }
    if (!window) {
      window = new RecoveryWindow(ctx.clock);
    } else if (accepted) {
      window.resume();
    }
    ctx.log({
      event: LogEvent.RELAY_RECOVERY,
      rpc: "ReadFile",
      sandboxId: ctx.sandboxId,
    });
    const delay = backoffMs(window.attempts++);
    if (window.remaining() <= delay) {
      throw lastError;
    }
    grant.discard();
    await ctx.clock.sleep(delay, signal);
  }
}
