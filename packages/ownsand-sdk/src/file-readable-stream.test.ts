// SPDX-License-Identifier: Apache-2.0
import { runInNewContext } from "node:vm";
import type { WriteFileRequest } from "@delino/ownsand-protos/v1/data_frame_pb";
import { describe, expect, test } from "@rstest/core";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SANDBOX, setup } from "./testing/harness.ts";

const REASON = new Error("caller aborted the write");

/**
 * Returns a readable stream that `instanceof ReadableStream` does not
 * recognize. Its prototype comes from a `node:vm` realm and carries only the
 * stream methods, so it models a stream that crossed a realm boundary or whose
 * prototype was replaced. The methods still work because the stream's
 * internal slots live on the instance.
 */
function foreignReadableStream(
  underlyingSource: UnderlyingDefaultSource<Uint8Array>,
  strategy?: QueuingStrategy<Uint8Array>,
): ReadableStream<Uint8Array> {
  const stream = new ReadableStream<Uint8Array>(underlyingSource, strategy);
  const proto = runInNewContext("({})") as object;
  const source = ReadableStream.prototype;
  Object.defineProperties(proto, {
    getReader: { value: source.getReader },
    cancel: { value: source.cancel },
    locked: Object.getOwnPropertyDescriptor(source, "locked")!,
    [Symbol.asyncIterator]: {
      value: (source as any)[Symbol.asyncIterator],
    },
  });
  Object.setPrototypeOf(stream, proto);
  return stream;
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

describe("file write from a readable stream of another realm", () => {
  test("the test stream is not recognized by instanceof", () => {
    const stream = foreignReadableStream({});
    expect(stream instanceof ReadableStream).toBe(false);
    expect(stream.locked).toBe(false);
  });

  test("an abort during a pending read rejects with the reason, cancels the source with it, releases the lock and sends no request", async () => {
    const h = setup();
    const pulled = deferred();
    let cancelled: unknown;
    const stream = foreignReadableStream({
      pull() {
        pulled.resolve();
        // The source never yields.
        return new Promise(() => {});
      },
      cancel(reason) {
        cancelled = reason;
        // A source whose cancel never settles must not delay the abort.
        return new Promise(() => {});
      },
    });
    const controller = new AbortController();
    const outcome = h.client
      .sandbox(SANDBOX)
      .writeFile("/f", stream, { signal: controller.signal })
      .then(
        () => "resolved",
        (error) => error,
      );
    await pulled.promise;
    expect(stream.locked).toBe(true);
    controller.abort(REASON);
    expect(await outcome).toBe(REASON);
    expect(stream.locked).toBe(false);
    expect(cancelled).toBe(REASON);
    expect(h.api.callsOf("WriteFile")).toHaveLength(0);
  });

  test("a pre-aborted signal rejects with the reason and sends a request", async () => {
    const h = setup();
    const stream = foreignReadableStream({});
    const controller = new AbortController();
    controller.abort(REASON);
    await expect(
      h.client
        .sandbox(SANDBOX)
        .writeFile("/f", stream, { signal: controller.signal }),
    ).rejects.toBe(REASON);
    expect(stream.locked).toBe(false);
    expect(h.api.calls).toHaveLength(0);
  });

  test("an abort after the first chunk was acknowledged stops at the next source wait, sends no final chunk and releases the lock", async () => {
    const h = setup();
    const writeEntered = deferred();
    const releaseAck = deferred();
    h.api.unary(RelayService, "WriteFile", async (req) => {
      writeEntered.resolve();
      await releaseAck.promise;
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: false,
        sizeBytes: 0n,
      };
    });
    const controller = new AbortController();
    const thirdPull = deferred();
    let pulls = 0;
    const stream = foreignReadableStream(
      {
        pull(c) {
          pulls += 1;
          // The second piece lets the SDK send the first chunk without finish.
          if (pulls <= 2) {
            c.enqueue(new Uint8Array(1 << 20));
            return;
          }
          thirdPull.resolve();
          return new Promise<void>(() => {});
        },
      },
      // Disable prefetch so every pull requires a pending read.
      { highWaterMark: 0 },
    );
    const outcome = h.client
      .sandbox(SANDBOX)
      .writeFile("/f", stream, { signal: controller.signal })
      .then(
        () => "resolved",
        (error: unknown) => error,
      );
    try {
      await h.clock.run(writeEntered.promise, 0);
      await h.clock.settle();
      expect(pulls).toBe(2);
      releaseAck.resolve();
      await h.clock.run(thirdPull.promise, 0);
    } finally {
      controller.abort(REASON);
      releaseAck.resolve();
    }
    expect(await h.clock.run(outcome, 0)).toBe(REASON);
    expect(stream.locked).toBe(false);
    const writes = h.api.callsOf("WriteFile");
    expect(writes).toHaveLength(1);
    const write = writes[0]!.request as WriteFileRequest;
    expect(write.offset).toBe(0n);
    expect(write.data).toHaveLength(1 << 20);
    expect(write.finish).toBeUndefined();
  });

  test("uploads the bytes of a completed stream", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req) => ({
      writeId: req.writeId,
      offset: req.offset,
      length: BigInt(req.data.length),
      finished: req.finish !== undefined,
      sizeBytes: req.finish?.sizeBytes ?? 0n,
    }));
    const stream = foreignReadableStream({
      start(c) {
        c.enqueue(Uint8Array.from([1, 2, 3]));
        c.enqueue(Uint8Array.from([4, 5]));
        c.close();
      },
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", stream));
    const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
    expect(
      Buffer.concat(writes.map((w) => Buffer.from(w.data))).toJSON().data,
    ).toEqual([1, 2, 3, 4, 5]);
    expect(writes.at(-1).finish.sizeBytes).toBe(5n);
    expect(stream.locked).toBe(false);
  });
});
