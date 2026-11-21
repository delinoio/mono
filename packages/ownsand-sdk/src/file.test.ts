// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { readFileBinding, requestDigest, writeFileBinding } from "./digest.ts";
import { DataPathError } from "./errors.ts";
import { RelayOperationKind } from "./gen/delino/ownsand/v1/common_pb.ts";
import { DataPathService } from "./gen/delino/ownsand/v1/data_path_pb.ts";
import { RelayService } from "./gen/delino/ownsand/v1/relay_pb.ts";
import { chunkSource } from "./operations/write.ts";
import { Disconnect, ownsandError, Trickle } from "./testing/fake-api.ts";
import { file, SANDBOX, setup, text } from "./testing/harness.ts";

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

describe("file write", () => {
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

  test("rejects a relative guest path before any request", async () => {
    const h = setup();
    await expect(
      h.client.sandbox(SANDBOX).writeFile("relative/path", "x"),
    ).rejects.toThrow("absolute");
    expect(() => h.client.sandbox(SANDBOX).readFile("x")).toThrow("absolute");
    expect(h.api.calls).toHaveLength(0);
  });
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
