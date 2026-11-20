// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";

import { bindingDigest, writeBinding } from "../digest.ts";
import { DataPathError, isInterruption, markInterruption } from "../errors.ts";
import { RelayOperationKind } from "../gen/delino/ownsand/v1/common_pb.ts";
import {
  type WriteFileRequest,
  WriteFileRequestSchema,
  type WriteFileResponse,
} from "../gen/delino/ownsand/v1/datapath_pb.ts";
import { newBindingSalt, newUuidV7 } from "../ids.ts";
import { LogEvent } from "../log.ts";
import type { RelayContext } from "./context.ts";
import { OperationGrant, WRITE_CALL_NEED_MS } from "./grant.ts";
import {
  backoffMs,
  linkedController,
  RecoveryWindow,
  WRITE_ATTEMPT_DEADLINE_MS,
} from "./recovery.ts";

/** Largest chunk of one `WriteFile` call. */
export const WRITE_CHUNK_BYTES = 1024 * 1024;

/** Input of `writeFile`: a byte buffer, a string (UTF-8) or a reader of byte chunks. */
export type WriteFileInput =
  | Uint8Array
  | ArrayBuffer
  | string
  | Iterable<Uint8Array>
  | AsyncIterable<Uint8Array | string>;

/**
 * Splits any input into chunks of at most 1 MiB, reading input lazily. A
 * reader's pieces are sent as they arrive (larger ones are split), so the SDK
 * never holds back data while it waits for more input.
 */
export async function* rechunk(
  input: WriteFileInput,
  size = WRITE_CHUNK_BYTES,
): AsyncGenerator<Uint8Array> {
  if (typeof input === "string") {
    input = new TextEncoder().encode(input);
  }
  if (input instanceof ArrayBuffer) {
    input = new Uint8Array(input);
  }
  const pieces:
    Iterable<Uint8Array | string> | AsyncIterable<Uint8Array | string> =
    input instanceof Uint8Array ? [input] : input;
  for await (const piece of pieces) {
    const bytes =
      typeof piece === "string" ? new TextEncoder().encode(piece) : piece;
    for (let i = 0; i < bytes.byteLength; i += size) {
      yield bytes.subarray(i, Math.min(i + size, bytes.byteLength));
    }
  }
}

/**
 * Writes one guest file over the relay in chunks of at most 1 MiB and
 * finishes it with its size and mode. The SDK keeps the unacknowledged
 * request and sends that exact request again after a failure before it reads
 * more input.
 */
export async function relayWriteFile(
  ctx: RelayContext,
  path: string,
  input: WriteFileInput,
  mode: number,
  signal: AbortSignal,
): Promise<WriteFileResponse> {
  // The write_id is generated right before the first call.
  const writeId = newUuidV7(ctx.clock.wallNow());
  const salt = newBindingSalt();
  const digest = bindingDigest(
    writeBinding(salt, ctx.sandboxId, writeId, path),
  );
  const grant = new OperationGrant(
    ctx,
    RelayOperationKind.WRITE_FILE,
    writeId,
    digest,
  );
  let offset = 0n;
  let first = true;
  const base = (): {
    organizationId: string;
    sandboxId: string;
    writeId: string;
    path?: string;
    bindingSalt?: Uint8Array;
  } => ({
    organizationId: ctx.organizationId,
    sandboxId: ctx.sandboxId,
    writeId,
    ...(first ? { path, bindingSalt: salt } : {}),
  });

  for await (const data of rechunk(input)) {
    const request = create(WriteFileRequestSchema, {
      ...base(),
      chunk: { offset, data },
    });
    await sendUntilAcknowledged(ctx, grant, request, signal);
    first = false;
    offset += BigInt(data.byteLength);
  }
  const finish = create(WriteFileRequestSchema, {
    ...base(),
    finish: { sizeBytes: offset, mode },
  });
  const res = await sendUntilAcknowledged(ctx, grant, finish, signal);
  if (!res.finished) {
    throw new DataPathError(
      "relay WriteFile did not confirm the finished write",
      Code.Internal,
    );
  }
  return res;
}

/**
 * Sends one `WriteFile` request until the agent acknowledges it. One recovery
 * budget of 60 seconds applies to the request: a new attempt starts only while
 * it lasts, an attempt that started in time may run until its SDK-side
 * deadline of 5 minutes 30 seconds, and only an acknowledgement resets it.
 */
async function sendUntilAcknowledged(
  ctx: RelayContext,
  grant: OperationGrant,
  request: WriteFileRequest,
  signal: AbortSignal,
): Promise<WriteFileResponse> {
  let budget: RecoveryWindow | undefined;
  for (;;) {
    signal.throwIfAborted();
    const grantId = await grant.acquire(WRITE_CALL_NEED_MS, signal, budget);
    const { controller, dispose } = linkedController(signal);
    const deadlineStop = new AbortController();
    let timedOut = false;
    const deadline = ctx.clock
      .sleep(WRITE_ATTEMPT_DEADLINE_MS, deadlineStop.signal)
      .then(
        () => {
          timedOut = true;
          controller.abort(
            new ConnectError(
              "relay WriteFile attempt passed its deadline",
              Code.DeadlineExceeded,
            ),
          );
        },
        () => {},
      );
    grant.streamOpened();
    let err: unknown;
    try {
      return await ctx.writeFile(grantId, request, {
        signal: controller.signal,
      });
    } catch (e) {
      err =
        timedOut && !signal.aborted
          ? markInterruption(
              new ConnectError(
                "relay WriteFile attempt passed its deadline",
                Code.DeadlineExceeded,
              ),
            )
          : e;
    } finally {
      grant.streamClosed();
      deadlineStop.abort();
      dispose();
      await deadline;
    }
    if (signal.aborted) {
      throw err;
    }
    const code = ConnectError.from(err).code;
    const retryable =
      isInterruption(err) ||
      code === Code.Unavailable ||
      code === Code.ResourceExhausted ||
      code === Code.DeadlineExceeded;
    if (!retryable) {
      throw err;
    }
    budget ??= new RecoveryWindow(ctx.clock);
    ctx.log({
      event: LogEvent.RELAY_RECOVERY,
      rpc: "WriteFile",
      sandboxId: ctx.sandboxId,
    });
    const delay = backoffMs(budget.attempts++);
    if (budget.remaining() <= delay) {
      throw err;
    }
    grant.discard();
    await ctx.clock.sleep(delay, signal);
  }
}
