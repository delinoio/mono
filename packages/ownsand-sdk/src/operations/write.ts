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
 * It reads one chunk ahead so that the last chunk is known. An abort of
 * `signal` ends the iteration with the signal's reason, also while the source
 * has not produced its next piece.
 */
export async function* chunkSource(
  source: WriteSource,
  size: number = MAX_FILE_CHUNK_SIZE,
  signal?: AbortSignal,
): AsyncGenerator<{ data: Uint8Array; last: boolean }> {
  // Unconsumed pieces plus a read offset into the first one. Each piece and
  // each chunk is copied once, so the total copy volume is linear in the
  // input size. The caller's buffers are never aliased: an unacknowledged
  // chunk can be resent after a failure while the caller mutates its own
  // buffer.
  const pieces: Uint8Array[] = [];
  let offset = 0;
  let pendingLength = 0;
  let held: Uint8Array | undefined;
  const take = (n: number): Uint8Array => {
    const out = new Uint8Array(n);
    let filled = 0;
    while (filled < n) {
      const head = pieces[0]!;
      const count = Math.min(n - filled, head.length - offset);
      out.set(head.subarray(offset, offset + count), filled);
      filled += count;
      offset += count;
      if (offset === head.length) {
        pieces.shift();
        offset = 0;
      }
    }
    pendingLength -= n;
    return out;
  };
  for await (const piece of iterateSource(source, signal)) {
    if (piece.length === 0) continue;
    // Own the piece before the loop asks the source for another one: a source
    // can reuse its buffer between yields, and the caller can change a buffer
    // while a chunk waits to be sent. Each byte is copied here once and in
    // `take` once, so the copy volume stays linear in the input size. The
    // constructor always copies: a Node.js `Buffer` overrides `slice` to
    // return a view over the same memory, so `piece.slice()` would alias it.
    pieces.push(new Uint8Array(piece));
    pendingLength += piece.length;
    while (pendingLength >= size) {
      if (held !== undefined) {
        yield { data: held, last: false };
      }
      held = take(size);
    }
  }
  if (pendingLength > 0) {
    if (held !== undefined) {
      yield { data: held, last: false };
    }
    held = take(pendingLength);
  }
  yield { data: held ?? new Uint8Array(0), last: true };
}

/**
 * Settles with `promise`, or rejects with the signal's reason as soon as the
 * signal aborts, even if `promise` never settles. The late outcome of
 * `promise` is ignored (and never reported as an unhandled rejection).
 */
function abortable<T>(
  promise: Promise<T>,
  signal: AbortSignal | undefined,
): Promise<T> {
  if (signal === undefined) return promise;
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(signal.reason);
    promise.then(
      (value) => {
        signal.removeEventListener("abort", onAbort);
        resolve(value);
      },
      (error) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
    if (signal.aborted) {
      onAbort();
    } else {
      signal.addEventListener("abort", onAbort, { once: true });
    }
  });
}

function ignoreFailure(): void {}

/**
 * Yields the pieces of a source. The signal is observed while the source is
 * idle: an abort rejects with the signal's reason at once, even when a pending
 * read or `next()` never settles, and the source is released without waiting
 * for it to cooperate.
 */
async function* iterateSource(
  source: WriteSource,
  signal: AbortSignal | undefined,
): AsyncGenerator<Uint8Array> {
  throwIfAborted(signal);
  if (typeof source === "string") {
    yield new TextEncoder().encode(source);
  } else if (source instanceof Uint8Array) {
    yield source;
  } else if (source instanceof ArrayBuffer) {
    yield new Uint8Array(source);
  } else if (source instanceof ReadableStream) {
    const reader = source.getReader();
    let finished = false;
    try {
      for (;;) {
        throwIfAborted(signal);
        const { done, value } = await abortable(reader.read(), signal);
        if (done) {
          finished = true;
          return;
        }
        yield value;
      }
    } finally {
      if (!finished) {
        // Cancel first: it settles a pending read, which a lock release
        // alone does not on every runtime. The source's own cancel callback
        // may never return, so the outcome is not awaited.
        reader.cancel(signal?.reason).catch(ignoreFailure);
      }
      try {
        reader.releaseLock();
      } catch {
        // A runtime that refuses to release a lock with a pending read has
        // already had the stream canceled above.
      }
    }
  } else {
    const iterable = source as Iterable<Uint8Array> | AsyncIterable<Uint8Array>;
    const iterator: Iterator<Uint8Array> | AsyncIterator<Uint8Array> =
      Symbol.asyncIterator in iterable
        ? iterable[Symbol.asyncIterator]()
        : (iterable as Iterable<Uint8Array>)[Symbol.iterator]();
    let finished = false;
    try {
      for (;;) {
        throwIfAborted(signal);
        const result = await abortable(
          Promise.resolve(iterator.next()),
          signal,
        );
        if (result.done) {
          finished = true;
          return;
        }
        const piece = await abortable(Promise.resolve(result.value), signal);
        yield piece instanceof Uint8Array ? piece : new Uint8Array(piece);
      }
    } finally {
      if (!finished) {
        // Request the iterator's cleanup but do not wait for it: an iterator
        // that is stuck in `next()` can never finish `return()` either.
        try {
          Promise.resolve(iterator.return?.()).catch(ignoreFailure);
        } catch {
          // A synchronous failure of the cleanup has no caller to report to.
        }
      }
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
  throwIfAborted(signal);
  for await (const chunk of chunkSource(source, MAX_FILE_CHUNK_SIZE, signal)) {
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
        if (session !== undefined) selector.sessionEnded(session);
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
    // The control plane's 5-minute call limit is retryable for a write,
    // which the write_id rules make safe. The grant decision and the
    // recovery rules must see the same (wrapped) failure.
    const retryable = asRetryableDeadline(failure);
    grants.discardFor(retryable);
    await recoverOrThrow(ctx, window, retryable, signal);
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
