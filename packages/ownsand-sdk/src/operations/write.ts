// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import { MAX_FILE_CHUNK_SIZE } from "../codec.ts";
import type { DataPathSelector } from "../datapath.ts";
import { newBindingSalt, requestDigest, writeFileBinding } from "../digest.ts";
import { DataPathError } from "../errors.ts";
import { RelayOperationKind } from "../gen/delino/ownsand/v1/common_pb.ts";
import {
  type WriteFileRequest,
  WriteFileRequestSchema,
  type WriteFileResponse,
} from "../gen/delino/ownsand/v1/data_frame_pb.ts";
import { PeerUnavailableError, SessionEndedError } from "../p2p/session.ts";
import {
  type OperationContext,
  Timing,
  throwIfAborted,
} from "../relay/context.ts";
import { GrantKeeper, RelayCallKind } from "../relay/grant.ts";
import { RecoveryWindow } from "../relay/recovery.ts";
import { relayCallWithDeadline } from "../relay/stream.ts";
import { recoverOrThrow } from "./common.ts";

/** Contents that `writeFile` accepts. */
export type WriteSource =
  | Uint8Array
  | ArrayBuffer
  | string
  | Iterable<Uint8Array>
  | AsyncIterable<Uint8Array>
  | ReadableStream<Uint8Array>;

/** Default mode of a written file. */
export const DEFAULT_FILE_MODE = 0o644;

/** The result of a finished write. */
export interface WriteFileResult {
  writeId: string;
  sizeBytes: bigint;
}

/**
 * Splits a source into chunks of exactly `size` bytes, except the last one.
 * It reads one chunk ahead so that the last chunk is known.
 */
export async function* chunkSource(
  source: WriteSource,
  size: number = MAX_FILE_CHUNK_SIZE,
): AsyncGenerator<{ data: Uint8Array; last: boolean }> {
  let pending = new Uint8Array(0);
  let held: Uint8Array | undefined;
  for await (const piece of iterateSource(source)) {
    const joined = new Uint8Array(pending.length + piece.length);
    joined.set(pending, 0);
    joined.set(piece, pending.length);
    pending = joined;
    while (pending.length >= size) {
      if (held !== undefined) {
        yield { data: held, last: false };
      }
      held = pending.slice(0, size);
      pending = pending.slice(size);
    }
  }
  if (pending.length > 0) {
    if (held !== undefined) {
      yield { data: held, last: false };
    }
    held = pending;
  }
  yield { data: held ?? new Uint8Array(0), last: true };
}

async function* iterateSource(source: WriteSource): AsyncGenerator<Uint8Array> {
  if (typeof source === "string") {
    yield new TextEncoder().encode(source);
  } else if (source instanceof Uint8Array) {
    yield source;
  } else if (source instanceof ArrayBuffer) {
    yield new Uint8Array(source);
  } else if (source instanceof ReadableStream) {
    const reader = source.getReader();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;
        yield value;
      }
    } finally {
      reader.releaseLock();
    }
  } else {
    for await (const piece of source as AsyncIterable<Uint8Array>) {
      yield piece instanceof Uint8Array ? piece : new Uint8Array(piece);
    }
  }
}

/**
 * Writes a guest file in chunks of at most 1 MiB, one unacknowledged chunk
 * at a time. The first call carries the path and salt; the last sets
 * `finish`. After a failure or a session end, the SDK sends the exact
 * unacknowledged request again before it reads more input.
 */
export async function runWriteFile(
  ctx: OperationContext,
  selector: DataPathSelector,
  path: string,
  source: WriteSource,
  mode: number,
  signal: AbortSignal | undefined,
): Promise<WriteFileResult> {
  const writeId = ctx.ids.next();
  const salt = newBindingSalt();
  let grants: GrantKeeper | undefined;
  let offset = 0n;
  let first = true;
  for await (const chunk of chunkSource(source)) {
    const request = create(WriteFileRequestSchema, {
      organizationId: ctx.organizationId,
      sandboxId: ctx.sandboxId,
      writeId,
      path: first ? path : "",
      bindingSalt: first ? salt : new Uint8Array(0),
      offset,
      data: chunk.data,
      finish: chunk.last
        ? { sizeBytes: offset + BigInt(chunk.data.length), mode }
        : undefined,
    });
    grants ??= new GrantKeeper(
      ctx,
      RelayOperationKind.WRITE_FILE,
      writeId,
      requestDigest(writeFileBinding(request)),
    );
    const response = await sendChunk(ctx, selector, grants, request, signal);
    if (
      response.offset !== request.offset ||
      response.length !== BigInt(request.data.length)
    ) {
      throw new DataPathError("the agent acknowledged a different range", {
        code: Code.Internal,
      });
    }
    offset += BigInt(chunk.data.length);
    first = false;
    if (chunk.last) {
      if (!response.finished) {
        throw new DataPathError("the agent did not finish the write", {
          code: Code.Internal,
        });
      }
      return { writeId, sizeBytes: response.sizeBytes };
    }
  }
  throw new DataPathError("the write source ended without a last chunk", {
    code: Code.Internal,
  });
}

/**
 * Sends one request until the agent acknowledges it. One recovery budget of
 * 60 seconds applies to the request; an attempt that started in time may run
 * until its own deadline.
 */
async function sendChunk(
  ctx: OperationContext,
  selector: DataPathSelector,
  grants: GrantKeeper,
  request: WriteFileRequest,
  signal: AbortSignal | undefined,
): Promise<WriteFileResponse> {
  const window = new RecoveryWindow(ctx.clock);
  for (;;) {
    throwIfAborted(signal);
    const session = await selector.select(signal);
    let failure: unknown;
    if (session !== undefined) {
      try {
        return await session.writeFile(request, signal);
      } catch (error) {
        failure = error;
      }
      throwIfAborted(signal);
      if (failure instanceof SessionEndedError) {
        selector.sessionEnded();
        continue;
      }
      if (failure instanceof PeerUnavailableError) {
        selector.peerFailed(failure);
        window.fail();
        continue;
      }
      await recoverOrThrow(ctx, window, failure, signal);
      continue;
    }
    const grantId = await grants.acquire(
      RelayCallKind.WriteChunk,
      signal,
      window.active ? window : undefined,
    );
    grants.streamStarted();
    try {
      return await relayCallWithDeadline(
        ctx,
        grantId,
        signal,
        Timing.writeAttemptDeadlineMs,
        (options) => ctx.relay.writeFile(request, options),
      );
    } catch (error) {
      failure = error;
    } finally {
      grants.streamEnded();
    }
    throwIfAborted(signal);
    grants.discard();
    // The control plane's 5-minute call limit is retryable for a write,
    // which the write_id rules make safe.
    await recoverOrThrow(ctx, window, asRetryableDeadline(failure), signal);
  }
}

function asRetryableDeadline(error: unknown): unknown {
  if (
    error instanceof Error &&
    "code" in error &&
    (error as { code: unknown }).code === Code.DeadlineExceeded
  ) {
    return new DataPathError("WriteFile call limit reached", {
      code: Code.DeadlineExceeded,
      cause: error,
    });
  }
  return error;
}
