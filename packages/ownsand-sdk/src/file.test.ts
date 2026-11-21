// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { readFileBinding, requestDigest, writeFileBinding } from "./digest.ts";
import { DataPathError, OwnSandValidationError } from "./errors.ts";
import { AcceptanceDeadlineError } from "./relay/stream.ts";
import { RelayOperationKind } from "./gen/delino/ownsand/v1/common_pb.ts";
import { DataPathService } from "./gen/delino/ownsand/v1/data_path_pb.ts";
import { RelayService } from "./gen/delino/ownsand/v1/relay_pb.ts";
import { chunkSource } from "./operations/write.ts";
import {
  Disconnect,
  Hang,
  ownsandError,
  Sleep,
  Trickle,
} from "./testing/fake-api.ts";
import { file, SANDBOX, setup, text } from "./testing/harness.ts";
import { validateGuestPath } from "./validate.ts";

const MIN = 60_000;

describe("file read", () => {
  test("streams chunks with the read_id, path and salt and checks the final size", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      yield file.accepted();
      yield file.chunk(0, "hello ");
      yield file.keepalive();
      yield file.chunk(6, "world");
      yield file.end(11);
    });
    const bytes = await h.clock.run(
      h.client.sandbox(SANDBOX).readFileBytes("/etc/motd"),
    );
    expect(text(bytes)).toBe("hello world");
    const read = h.api.callsOf("ReadFile")[0]!.request as any;
    const auth = h.api.callsOf("AuthorizeRelay")[0]!.request as any;
    expect(read.path).toBe("/etc/motd");
    expect(read.bindingSalt).toHaveLength(32);
    expect(auth.operationKind).toBe(RelayOperationKind.READ_FILE);
    expect(auth.operationId).toBe(read.readId);
    expect(Buffer.from(auth.requestDigest)).toEqual(
      Buffer.from(requestDigest(readFileBinding(read))),
    );
  });

  test("resumes at the next offset with the same read_id after an abrupt disconnect", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* (req, call) {
      yield file.accepted();
      if (call.index === 0) {
        yield file.chunk(0, "abc");
        yield new Disconnect();
        return;
      }
      expect(req.offset).toBe(3n);
      // The agent resends one delivered byte; the SDK drops it.
      yield file.chunk(2, "cdef");
      yield file.end(6);
    });
    const bytes = await h.clock.run(
      h.client.sandbox(SANDBOX).readFileBytes("/data"),
    );
    expect(text(bytes)).toBe("abcdef");
    const reads = h.api.callsOf("ReadFile").map((c) => c.request as any);
    expect(reads[1].readId).toBe(reads[0].readId);
    expect(Buffer.from(reads[1].bindingSalt)).toEqual(
      Buffer.from(reads[0].bindingSalt),
    );
    expect(reads[1].offset).toBe(3n);
  });

  test("continues a rotated read stream without an error", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* (_req, call) {
      yield file.accepted();
      if (call.index === 0) {
        yield file.chunk(0, "ab");
        return;
      }
      yield file.chunk(2, "cd");
      yield file.end(4);
    });
    const bytes = await h.clock.run(
      h.client.sandbox(SANDBOX).readFileBytes("/data"),
    );
    expect(text(bytes)).toBe("abcd");
  });

  test("an expired read_id is a visible error", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      throw new ConnectError("read expired", Code.NotFound);
    });
    const error = await h.clock
      .run(h.client.sandbox(SANDBOX).readFileBytes("/data"))
      .catch((e) => e);
    expect(error).toBeInstanceOf(DataPathError);
    expect(error.code).toBe(Code.NotFound);
  });

  test("repeated accepted continuations without data at the same offset end within the window", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", async function* (_req, call) {
      yield file.accepted();
      if (call.index === 0) {
        yield file.chunk(0, "a");
      }
      yield new Disconnect();
    });
    const error = await h.clock
      .run(h.client.sandbox(SANDBOX).readFileBytes("/data"))
      .catch((e) => e);
    expect(error).toBeDefined();
    expect(h.clock.now()).toBeLessThanOrEqual(61_000);
    const offsets = h.api
      .callsOf("ReadFile")
      .slice(1)
      .map((c) => (c.request as any).offset);
    expect(new Set(offsets)).toEqual(new Set([1n]));
  });

  describe("a recovery read that the agent does not accept", () => {
    test("ends at the remaining window while only keepalives arrive", async () => {
      const h = setup();
      let continuation: AbortSignal | undefined;
      h.api.stream(RelayService, "ReadFile", async function* (_req, call) {
        if (call.index === 0) {
          yield file.accepted();
          yield file.chunk(0, "a");
          yield new Disconnect();
          return;
        }
        continuation = call.signal;
        for (;;) {
          yield file.keepalive();
          yield new Sleep(10_000);
        }
      });
      const error = await h.clock
        .run(h.client.sandbox(SANDBOX).readFileBytes("/data"))
        .catch((e) => e);
      expect(error).toBeInstanceOf(AcceptanceDeadlineError);
      expect(error.code).toBe(Code.Unavailable);
      expect(h.clock.now()).toBeGreaterThanOrEqual(MIN);
      expect(h.clock.now()).toBeLessThan(MIN + 1_000);
      expect(h.api.callsOf("ReadFile")).toHaveLength(2);
      expect(continuation?.aborted).toBe(true);
    });

    test("ends at the remaining window while no frame arrives", async () => {
      const h = setup();
      h.api.stream(RelayService, "ReadFile", async function* (_req, call) {
        if (call.index === 0) {
          yield new Disconnect();
          return;
        }
        yield new Hang();
      });
      const error = await h.clock
        .run(h.client.sandbox(SANDBOX).readFileBytes("/data"))
        .catch((e) => e);
      expect(error).toBeInstanceOf(AcceptanceDeadlineError);
      expect(h.clock.now()).toBeGreaterThanOrEqual(MIN);
      expect(h.clock.now()).toBeLessThan(MIN + 1_000);
      expect(h.api.callsOf("ReadFile")).toHaveLength(2);
    });

    test("keeps running past the window when accepted arrives in time", async () => {
      const h = setup();
      h.api.stream(RelayService, "ReadFile", async function* (_req, call) {
        if (call.index === 0) {
          yield new Disconnect();
          return;
        }
        yield new Sleep(30_000);
        yield file.accepted();
        for (let i = 0; i < 12; i++) {
          yield new Sleep(10_000);
          yield file.keepalive();
        }
        yield file.chunk(0, "ok");
        yield file.end(2);
      });
      const bytes = await h.clock.run(
        h.client.sandbox(SANDBOX).readFileBytes("/data"),
      );
      expect(text(bytes)).toBe("ok");
      expect(h.clock.now()).toBeGreaterThan(MIN + 30_000);
    });
  });

  test("a recovery read whose first chunk takes longer than 90 seconds while bytes keep arriving", async () => {
    const h = setup();
    const big = new Uint8Array(1 << 20).fill(65);
    h.api.stream(RelayService, "ReadFile", async function* (_req, call) {
      if (call.index === 0) {
        yield new Disconnect();
        return;
      }
      yield file.accepted();
      // The 1 MiB chunk arrives in pieces 30 seconds apart: 150 seconds in
      // total, but the stream never goes 90 seconds without a byte.
      yield new Trickle(file.chunk(0, big), 6, 30_000);
      yield file.end(big.length);
    });
    const bytes = await h.clock.run(
      h.client.sandbox(SANDBOX).readFileBytes("/big"),
    );
    expect(bytes.length).toBe(big.length);
    expect(h.api.callsOf("ReadFile")).toHaveLength(2);
    expect(h.clock.now()).toBeGreaterThanOrEqual(150_000);
  });
});

describe("file read at agent capacity", () => {
  test("keeps the grant after a stream rejected at capacity and renews it after unavailable", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* (_req, call) {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "agent stream capacity");
      }
      if (call.index === 1) {
        throw ownsandError(Code.Unavailable, "agent unreachable");
      }
      yield file.accepted();
      yield file.chunk(0, "hi");
      yield file.end(2);
    });
    const bytes = await h.clock.run(
      h.client.sandbox(SANDBOX).readFileBytes("/f"),
    );
    expect(text(bytes)).toBe("hi");
    const reads = h.api.callsOf("ReadFile");
    expect(reads).toHaveLength(3);
    // The capacity rejection reuses grant-1; unavailable renews it.
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(auths).toHaveLength(2);
    expect(auths[1].replacesGrantId).toBe("grant-1");
    expect(reads.map((c) => c.headers.get("ownsand-relay-grant-id"))).toEqual([
      "grant-1",
      "grant-1",
      "grant-2",
    ]);
  });
});

describe("file write", () => {
  test("keeps the grant when a chunk is rejected at agent capacity", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req, call) => {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "agent capacity");
      }
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: true,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      };
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"));
    const writes = h.api.callsOf("WriteFile");
    expect(writes).toHaveLength(2);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
    for (const write of writes) {
      expect(write.headers.get("ownsand-relay-grant-id")).toBe("grant-1");
    }
  });

  test("renews the grant after a chunk call hits the call limit", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req, call) => {
      if (call.index === 0) {
        throw new ConnectError("relay call limit", Code.DeadlineExceeded);
      }
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: true,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      };
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"));
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(auths).toHaveLength(2);
    expect(auths[1].replacesGrantId).toBe("grant-1");
  });

  test("uploads in chunks of at most 1 MiB and finishes only with the last chunk", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req) => ({
      writeId: req.writeId,
      offset: req.offset,
      length: BigInt(req.data.length),
      finished: req.finish !== undefined,
      sizeBytes: req.finish?.sizeBytes ?? 0n,
    }));
    const data = new Uint8Array(2.5 * (1 << 20)).map((_, i) => i % 251);
    const result = await h.clock.run(
      h.client.sandbox(SANDBOX).writeFile("/out.bin", data, { mode: 0o600 }),
    );
    const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
    expect(writes.map((w) => w.offset)).toEqual([0n, 1048576n, 2097152n]);
    expect(writes.map((w) => w.data.length)).toEqual([
      1048576, 1048576, 524288,
    ]);
    expect(writes[0].path).toBe("/out.bin");
    expect(writes[0].bindingSalt).toHaveLength(32);
    expect(writes[1].path).toBe("");
    expect(writes[1].bindingSalt).toHaveLength(0);
    expect(writes.map((w) => w.finish !== undefined)).toEqual([
      false,
      false,
      true,
    ]);
    expect(writes[2].finish.sizeBytes).toBe(BigInt(data.length));
    expect(writes[2].finish.mode).toBe(0o600);
    expect(new Set(writes.map((w) => w.writeId)).size).toBe(1);
    expect(result.sizeBytes).toBe(BigInt(data.length));
    const auth = h.api.callsOf("AuthorizeRelay")[0]!.request as any;
    expect(auth.operationKind).toBe(RelayOperationKind.WRITE_FILE);
    expect(Buffer.from(auth.requestDigest)).toEqual(
      Buffer.from(requestDigest(writeFileBinding(writes[0]))),
    );
    // One grant for the whole write.
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("resends the exact unacknowledged chunk after deadline_exceeded", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req, call) => {
      if (call.index === 0) {
        throw new ConnectError("relay call limit", Code.DeadlineExceeded);
      }
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: true,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      };
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"));
    const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
    expect(writes).toHaveLength(2);
    expect(writes[1]).toEqual(writes[0]);
  });

  test("cancels an attempt without a response at the SDK-side deadline and retries it", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", async (req, call) => {
      if (call.index === 0) {
        await new Promise<void>((resolve) =>
          call.signal.addEventListener("abort", () => resolve(), {
            once: true,
          }),
        );
        throw new Error("aborted");
      }
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: true,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      };
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "x"));
    expect(h.api.callsOf("WriteFile")).toHaveLength(2);
    expect(h.clock.now()).toBeGreaterThanOrEqual(5 * MIN + 30_000);
  });

  test("renews the grant before a chunk that follows a three-minute pause", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req) => ({
      writeId: req.writeId,
      offset: req.offset,
      length: BigInt(req.data.length),
      finished: req.finish !== undefined,
      sizeBytes: req.finish?.sizeBytes ?? 0n,
    }));
    const clock = h.clock;
    async function* slowSource() {
      yield new Uint8Array(1 << 20);
      yield new Uint8Array(1 << 20);
      await clock.sleep(3 * MIN);
      yield new Uint8Array(10);
    }
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", slowSource()));
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(auths).toHaveLength(2);
    expect(auths[1].replacesGrantId).toBe("grant-1");
    const headers = h.api
      .callsOf("WriteFile")
      .map((c) => c.headers.get("ownsand-relay-grant-id"));
    // The second chunk waits for the read-ahead of the third, so it is sent
    // after the pause under the renewed grant.
    expect(headers).toEqual(["grant-1", "grant-2", "grant-2"]);
  });

  test("renews before a chunk whose grant would expire during the call", async () => {
    const h = setup();
    h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => ({
      grantId: `grant-${call.index + 1}`,
      // 5 minutes 40 seconds: enough for the first call, but after it the
      // grant would expire during a 5-minute call plus 30 seconds.
      expiresIn: { seconds: call.index === 0 ? 340n : 900n },
    }));
    h.api.unary(RelayService, "WriteFile", async (req) => {
      await h.clock.sleep(40_000);
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: req.finish !== undefined,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      };
    });
    await h.clock.run(
      h.client.sandbox(SANDBOX).writeFile("/f", new Uint8Array((1 << 20) + 1)),
    );
    const headers = h.api
      .callsOf("WriteFile")
      .map((c) => c.headers.get("ownsand-relay-grant-id"));
    expect(headers).toEqual(["grant-1", "grant-2"]);
  });

  test("retries a chunk after unavailable within its 60-second budget", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req, call) => {
      if (call.index < 2) {
        throw ownsandError(Code.Unavailable, "agent channel lost");
      }
      return {
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: true,
        sizeBytes: 1n,
      };
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "x"));
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(auths.map((a) => a.replacesGrantId)).toEqual([
      undefined,
      "grant-1",
      "grant-2",
    ]);
  });

  test("writes an empty file with one finishing call", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req) => ({
      writeId: req.writeId,
      offset: 0n,
      length: 0n,
      finished: true,
      sizeBytes: 0n,
    }));
    await h.clock.run(
      h.client.sandbox(SANDBOX).writeFile("/empty", new Uint8Array(0)),
    );
    const [write] = h.api.callsOf("WriteFile").map((c) => c.request as any);
    expect(write.finish.sizeBytes).toBe(0n);
    expect(write.path).toBe("/empty");
  });

  describe("cancellation while the source is waiting", () => {
    const REASON = new Error("caller gave up");

    test("a pre-aborted signal rejects with the reason and neither pulls the source nor sends a request", async () => {
      const h = setup();
      // A stream starts one prefetch pull by itself, so only the pulls that
      // follow the first one count as reads by the SDK.
      let pulled = 0;
      const stream = new ReadableStream<Uint8Array>({
        pull() {
          pulled++;
        },
      });
      await new Promise((resolve) => setTimeout(resolve, 10));
      const prefetched = pulled;
      pulled = 0;
      const iterable: AsyncIterable<Uint8Array> = {
        [Symbol.asyncIterator]() {
          pulled++;
          return { next: () => new Promise(() => {}) };
        },
      };
      for (const source of [stream, iterable, "text", new Uint8Array(2)]) {
        const controller = new AbortController();
        controller.abort(REASON);
        await expect(
          h.client
            .sandbox(SANDBOX)
            .writeFile("/f", source, { signal: controller.signal }),
        ).rejects.toBe(REASON);
      }
      expect(prefetched).toBe(1);
      expect(pulled).toBe(0);
      expect(stream.locked).toBe(false);
      expect(h.api.calls).toHaveLength(0);
    });

    test("an abort during a pending stream read rejects with the reason, releases the lock and sends no final chunk", async () => {
      const h = setup();
      let cancelled: unknown;
      const stream = new ReadableStream<Uint8Array>({
        pull() {
          return new Promise(() => {});
        },
        cancel(reason) {
          cancelled = reason;
          // A source whose cancel never returns must not delay the abort.
          return new Promise(() => {});
        },
      });
      const controller = new AbortController();
      const write = h.client
        .sandbox(SANDBOX)
        .writeFile("/f", stream, { signal: controller.signal });
      const outcome = write.then(
        () => "resolved",
        (error) => error,
      );
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(stream.locked).toBe(true);
      controller.abort(REASON);
      expect(await outcome).toBe(REASON);
      expect(stream.locked).toBe(false);
      expect(cancelled).toBe(REASON);
      expect(h.api.callsOf("WriteFile")).toHaveLength(0);
    });

    test("an abort during a pending iterator next() rejects with the reason and requests return() without waiting for it", async () => {
      const h = setup();
      let returned = 0;
      const iterable: AsyncIterable<Uint8Array> = {
        [Symbol.asyncIterator]() {
          return {
            next: () => new Promise(() => {}),
            return: () => {
              returned++;
              return new Promise(() => {});
            },
          };
        },
      };
      const controller = new AbortController();
      const outcome = h.client
        .sandbox(SANDBOX)
        .writeFile("/f", iterable, { signal: controller.signal })
        .then(
          () => "resolved",
          (error) => error,
        );
      await new Promise((resolve) => setTimeout(resolve, 10));
      controller.abort(REASON);
      expect(await outcome).toBe(REASON);
      expect(returned).toBe(1);
      expect(h.api.callsOf("WriteFile")).toHaveLength(0);
    });

    test("an abort after the first chunk was acknowledged stops at the next source wait and sends no final chunk", async () => {
      const h = setup();
      h.api.unary(RelayService, "WriteFile", (req) => ({
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: false,
        sizeBytes: 0n,
      }));
      const controller = new AbortController();
      async function* source() {
        yield new Uint8Array(1 << 20);
        yield new Uint8Array(1 << 20);
        controller.abort(REASON);
        await new Promise(() => {});
      }
      await expect(
        h.client
          .sandbox(SANDBOX)
          .writeFile("/f", source(), { signal: controller.signal }),
      ).rejects.toBe(REASON);
      const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
      expect(writes.every((w) => w.finish === undefined)).toBe(true);
    });

    test("an unmodified stream write without an abort sends the same chunks", async () => {
      const h = setup();
      h.api.unary(RelayService, "WriteFile", (req) => ({
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: req.finish !== undefined,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      }));
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(new TextEncoder().encode("hel"));
          controller.enqueue(new TextEncoder().encode("lo"));
          controller.close();
        },
      });
      const controller = new AbortController();
      const result = await h.clock.run(
        h.client
          .sandbox(SANDBOX)
          .writeFile("/f", stream, { signal: controller.signal }),
      );
      expect(result.sizeBytes).toBe(5n);
      const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
      expect(writes).toHaveLength(1);
      expect(text(writes[0].data)).toBe("hello");
      expect(stream.locked).toBe(false);
    });
  });

  test("rejects a relative guest path before any request", async () => {
    const h = setup();
    await expect(
      h.client.sandbox(SANDBOX).writeFile("relative/path", "x"),
    ).rejects.toThrow("absolute");
    expect(() => h.client.sandbox(SANDBOX).readFile("x")).toThrow("absolute");
    expect(h.api.calls).toHaveLength(0);
  });

  test.each(["\\file.txt", "\\x", "\\", "\\?x"])(
    "rejects root-relative Windows path %j without echoing it",
    async (path) => {
      const h = setup();
      const err = await h.client
        .sandbox(SANDBOX)
        .writeFile(path, "x")
        .then(
          () => undefined,
          (e: unknown) => e,
        );
      expect(err).toBeInstanceOf(OwnSandValidationError);
      expect((err as OwnSandValidationError).field).toBe("path");
      expect((err as Error).message).toContain("absolute");
      expect((err as Error).message).not.toContain("file.txt");
      expect(() => h.client.sandbox(SANDBOX).readFile(path)).toThrow(
        "absolute",
      );
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test.each([
    "/x",
    "C:\\x",
    "C:/x",
    "\\\\server\\share",
    "\\/server/share",
    "\\??\\C:\\x",
    "\\\\?\\C:\\x",
    "\\\\",
    "\\??\\",
    "//x",
  ])("accepts absolute guest path %j", (path) => {
    expect(() => validateGuestPath("path", path)).not.toThrow();
  });

  test.each(["", "relative/file", "C:", "C:x"])(
    "rejects non-absolute guest path %j",
    (path) => {
      expect(() => validateGuestPath("path", path)).toThrow();
    },
  );
});

describe("chunkSource", () => {
  test("splits a stream into exact chunks and marks the last", async () => {
    async function* pieces() {
      yield new Uint8Array(3);
      yield new Uint8Array(4);
      yield new Uint8Array(3);
    }
    const out: [number, boolean][] = [];
    for await (const c of chunkSource(pieces(), 4)) {
      out.push([c.data.length, c.last]);
    }
    expect(out).toEqual([
      [4, false],
      [4, false],
      [2, true],
    ]);
  });

  test("emits one empty last chunk for an empty source", async () => {
    const out = [];
    for await (const c of chunkSource(new Uint8Array(0), 4)) out.push(c);
    expect(out).toHaveLength(1);
    expect(out[0]!.last).toBe(true);
  });

  test("ends an exact multiple with a full last chunk", async () => {
    const out: [number, boolean][] = [];
    for await (const c of chunkSource(new Uint8Array(8), 4)) {
      out.push([c.data.length, c.last]);
    }
    expect(out).toEqual([
      [4, false],
      [4, true],
    ]);
  });

  test("keeps pending bytes when the source reuses its buffer between pieces", async () => {
    // A source such as a file reader can refill one buffer for every piece.
    // The bytes that wait for the next chunk must not change with the refill.
    const reused = new Uint8Array(2);
    async function* refill() {
      for (const value of [1, 2, 3]) {
        reused.fill(value);
        yield reused;
      }
    }
    const chunks: { data: number[]; last: boolean }[] = [];
    for await (const c of chunkSource(refill(), 4)) {
      chunks.push({ data: [...c.data], last: c.last });
    }
    expect(chunks).toEqual([
      { data: [1, 1, 2, 2], last: false },
      { data: [3, 3], last: true },
    ]);
  });

  test("keeps pending bytes when the source reuses a Node.js Buffer between pieces", async () => {
    // `Buffer.prototype.slice` returns a view, so a copy made with `slice`
    // would still alias the reused Buffer.
    const reused = Buffer.alloc(2);
    async function* refill() {
      for (const value of [1, 2]) {
        reused.fill(value);
        yield reused;
      }
    }
    const chunks: { data: number[]; last: boolean }[] = [];
    for await (const c of chunkSource(refill(), 4)) {
      chunks.push({ data: [...c.data], last: c.last });
    }
    expect(chunks).toEqual([{ data: [1, 1, 2, 2], last: true }]);
  });

  test("keeps the next chunks when the caller overwrites a Buffer input after the first chunk", async () => {
    const input = Buffer.from([1, 1, 1, 1, 2, 2, 2, 2, 3]);
    const seen: number[][] = [];
    for await (const c of chunkSource(input, 4)) {
      seen.push([...c.data]);
      input.fill(9);
    }
    expect(seen).toEqual([[1, 1, 1, 1], [2, 2, 2, 2], [3]]);
  });

  test("keeps a yielded chunk when the caller overwrites its buffer afterwards", async () => {
    const input = new Uint8Array([1, 1, 1, 1, 2, 2, 2, 2, 3]);
    const seen: number[][] = [];
    for await (const c of chunkSource(input, 4)) {
      seen.push([...c.data]);
      input.fill(9);
    }
    expect(seen).toEqual([[1, 1, 1, 1], [2, 2, 2, 2], [3]]);
  });

  test("copies a bounded volume for large single-piece and split inputs", async () => {
    const size = 1024;
    const total = 64 * 1024 * 1024;
    const input = new Uint8Array(total);
    for (let i = 0; i < total; i += 4099) input[i] = (i / 4099) & 0xff;

    async function* split() {
      const lengths = [size - 1, size + 1, 3 * size + 7, 17, 5 * size];
      let at = 0;
      for (let n = 0; at < total; n++) {
        const len = Math.min(lengths[n % lengths.length]!, total - at);
        yield input.subarray(at, at + len);
        at += len;
      }
    }

    const slice = Uint8Array.prototype.slice;
    const set = Uint8Array.prototype.set;
    let copied = 0;
    Uint8Array.prototype.slice = function (
      this: Uint8Array,
      ...args: Parameters<Uint8Array["slice"]>
    ) {
      const out = slice.apply(this, args);
      copied += out.length;
      return out;
    } as typeof slice;
    Uint8Array.prototype.set = function (
      this: Uint8Array,
      array: ArrayLike<number>,
      offset?: number,
    ) {
      copied += array.length;
      return set.call(this, array, offset);
    } as typeof set;

    try {
      for (const source of [input, split()]) {
        copied = 0;
        const chunks: Uint8Array[] = [];
        let lasts = 0;
        for await (const c of chunkSource(source, size)) {
          chunks.push(c.data);
          if (c.last) lasts++;
        }
        expect(copied).toBeLessThanOrEqual(2 * total);
        expect(lasts).toBe(1);
        expect(chunks.slice(0, -1).every((c) => c.length === size)).toBe(true);
        expect(chunks.reduce((n, c) => n + c.length, 0)).toBe(total);
        const joined = new Uint8Array(total);
        let at = 0;
        for (const c of chunks) {
          joined.set(c, at);
          at += c.length;
        }
        expect(Buffer.compare(joined, input)).toBe(0);
      }
    } finally {
      Uint8Array.prototype.slice = slice;
      Uint8Array.prototype.set = set;
    }
  });
});
