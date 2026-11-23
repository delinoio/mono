// SPDX-License-Identifier: Apache-2.0
import { runInNewContext } from "node:vm";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { readFileBinding, requestDigest, writeFileBinding } from "./digest.ts";
import type { Clock } from "./clock.ts";
import { DataPathError, OwnSandValidationError } from "./errors.ts";
import { Timing } from "./relay/context.ts";
import { AcceptanceDeadlineError } from "./relay/stream.ts";
import { RelayOperationKind } from "@delino/ownsand-protos/v1/common_pb";
import { DataPathService } from "@delino/ownsand-protos/v1/data_path_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { chunkSource, type WriteSource } from "./operations/write.ts";
import {
  Disconnect,
  Hang,
  ownsandError,
  Sleep,
  Trickle,
} from "./testing/fake-api.ts";
import { file, SANDBOX, setup, text } from "./testing/harness.ts";
import {
  expectTransportInterruption,
  expectWindowExhausted,
} from "./testing/recovery-assert.ts";
import type { VirtualClock } from "./testing/virtual-clock.ts";
import { uuidV7TimestampMs } from "./uuidv7.ts";
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

  test("rejects with DataLoss and does not retry when the end event reports a different size", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      yield file.accepted();
      yield file.chunk(0, "hello");
      yield file.end(7);
    });
    const failure = await h.clock
      .run(h.client.sandbox(SANDBOX).readFileBytes("/etc/motd"))
      .then(
        () => undefined,
        (e: unknown) => e,
      );
    expect(failure).toBeInstanceOf(DataPathError);
    expect((failure as DataPathError).code).toBe(Code.DataLoss);
    expect((failure as DataPathError).message).toContain(
      "file read ended at a size that differs from the delivered bytes",
    );
    expect(h.api.callsOf("ReadFile")).toHaveLength(1);
  });

  test("streams the delivered chunks before the size mismatch rejects the iteration", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      yield file.accepted();
      yield file.chunk(0, "hello");
      yield file.end(7);
    });
    const seen: string[] = [];
    const failure = await h.clock
      .run(
        (async () => {
          for await (const chunk of h.client
            .sandbox(SANDBOX)
            .readFile("/etc/motd")) {
            seen.push(text(chunk));
          }
        })(),
      )
      .then(
        () => undefined,
        (e: unknown) => e,
      );
    expect(seen).toEqual(["hello"]);
    expect(failure).toBeInstanceOf(DataPathError);
    expect((failure as DataPathError).code).toBe(Code.DataLoss);
    expect(h.api.callsOf("ReadFile")).toHaveLength(1);
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

  for (const guestMessage of [
    "read expired",
    "operation expired, or the caller's clock may be behind",
  ]) {
    test(`an expired read_id is a visible error ("${guestMessage}")`, async () => {
      const h = setup();
      h.api.stream(RelayService, "ReadFile", function* () {
        throw new ConnectError(guestMessage, Code.NotFound);
      });
      const error = await h.clock
        .run(h.client.sandbox(SANDBOX).readFileBytes("/data"))
        .catch((e) => e);
      expect(error).toBeInstanceOf(DataPathError);
      expect(error.code).toBe(Code.NotFound);
      expect(error.message).toBe("the file read expired");
      expect(error.cause).toBeInstanceOf(ConnectError);
      expect(error.cause.rawMessage).toBe(guestMessage);
    });
  }

  test("a missing file is a not_found error that keeps the guest message", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      throw new ConnectError("read: file does not exist", Code.NotFound);
    });
    const reads: Array<() => Promise<unknown>> = [
      () => h.client.sandbox(SANDBOX).readFileBytes("/missing"),
      async () => {
        for await (const _ of h.client.sandbox(SANDBOX).readFile("/missing")) {
          // No chunk arrives.
        }
      },
    ];
    for (const read of reads) {
      const error: any = await h.clock
        .run(read() as Promise<void>)
        .catch((e) => e);
      expect(error).toBeInstanceOf(DataPathError);
      expect(error.code).toBe(Code.NotFound);
      expect(error.message).toContain("file does not exist");
      expect(error.message).not.toContain("expired");
      expect(error.cause).toBeInstanceOf(ConnectError);
      expect(error.cause.rawMessage).toBe("read: file does not exist");
    }
    // A missing file is terminal: no recovery attempt.
    expect(h.api.callsOf("ReadFile")).toHaveLength(2);
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
    // The operation ends with the last transport interruption, not with a
    // helper error such as the virtual clock's deadlock.
    expectTransportInterruption(error);
    // Every call ends in a failure and every failure but the last one waits
    // one back-off delay; the last one finds no time left in the window.
    const calls = h.api.callsOf("ReadFile");
    expectWindowExhausted(h.clock.now(), calls.length - 1);
    const offsets = calls.slice(1).map((c) => (c.request as any).offset);
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

  /**
   * A clock whose first back-off sleep wakes `lateByMs` after its time, as a
   * suspended process or a starved event loop does. The monotonic time jumps
   * by that amount when the sleep resolves.
   */
  function lateWakeClock(clock: VirtualClock, lateByMs: number): Clock {
    let skew = 0;
    let slept = false;
    return {
      now: () => clock.now() + skew,
      wallNow: () => clock.wallNow() + skew,
      async sleep(ms, signal) {
        await clock.sleep(ms, signal);
        if (!slept) {
          slept = true;
          skew += lateByMs;
        }
      },
    };
  }

  // The wake that uses up the whole 60 second recovery window while the idle
  // time of the kept grant is exactly at its limit (not longer), so that
  // `acquire` returns the kept grant without renewing it.
  const KEPT_GRANT_LATE_MS = Timing.recoveryWindowMs - Timing.backoffInitialMs;

  function acknowledge(req: any) {
    return {
      writeId: req.writeId,
      offset: req.offset,
      length: BigInt(req.data.length),
      finished: true,
      sizeBytes: req.finish?.sizeBytes ?? 0n,
    };
  }

  test("starts no WriteFile when the back-off wakes after the recovery window ended and returns the last failure", async () => {
    const h = setup((clock) => ({
      clock: lateWakeClock(clock, KEPT_GRANT_LATE_MS),
    }));
    h.api.unary(RelayService, "WriteFile", (req, call) => {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "agent capacity");
      }
      return acknowledge(req);
    });
    const error: any = await h.clock
      .run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"))
      .catch((e) => e);
    expect(error).toBeInstanceOf(Error);
    expect(error.message).toContain("agent capacity");
    // The kept grant (resource_exhausted keeps it) must not send a second call.
    expect(h.api.callsOf("WriteFile")).toHaveLength(1);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("sends the retry when the back-off wakes inside the recovery window", async () => {
    const h = setup((clock) => ({ clock: lateWakeClock(clock, 30_000) }));
    h.api.unary(RelayService, "WriteFile", (req, call) => {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "agent capacity");
      }
      return acknowledge(req);
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"));
    expect(h.api.callsOf("WriteFile")).toHaveLength(2);
  });

  test("keeps the per-attempt deadline for a retry that starts in time", async () => {
    const h = setup((clock) => ({ clock: lateWakeClock(clock, 30_000) }));
    h.api.unary(RelayService, "WriteFile", async (req, call) => {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "agent capacity");
      }
      // The retry started in time, so it may run far beyond the 60 second
      // recovery budget, up to the 5 min 30 s attempt deadline.
      await h.clock.sleep(5 * MIN + 20_000);
      return acknowledge(req);
    });
    await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"));
    expect(h.api.callsOf("WriteFile")).toHaveLength(2);
    expect(h.clock.now()).toBeGreaterThanOrEqual(5 * MIN + 20_000);
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

  describe("binary sources from another realm", () => {
    // A `node:vm` context has its own Uint8Array and ArrayBuffer constructors,
    // so `instanceof` does not recognize its values.
    const foreign = <T>(code: string): T => runInNewContext(code) as T;

    function uploaded(h: ReturnType<typeof setup>): Uint8Array {
      const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
      return Buffer.concat(writes.map((w) => Buffer.from(w.data)));
    }

    async function upload(source: WriteSource): Promise<{
      bytes: Uint8Array;
      sizes: number[];
      finish: any;
    }> {
      const h = setup();
      h.api.unary(RelayService, "WriteFile", (req) => ({
        writeId: req.writeId,
        offset: req.offset,
        length: BigInt(req.data.length),
        finished: req.finish !== undefined,
        sizeBytes: req.finish?.sizeBytes ?? 0n,
      }));
      await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", source));
      const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
      return {
        bytes: uploaded(h),
        sizes: writes.map((w) => w.data.length),
        finish: writes.at(-1).finish,
      };
    }

    test("a foreign realm is not recognized by instanceof", () => {
      expect(foreign<unknown>("new Uint8Array(1)") instanceof Uint8Array).toBe(
        false,
      );
      expect(
        foreign<unknown>("new ArrayBuffer(1)") instanceof ArrayBuffer,
      ).toBe(false);
    });

    test("uploads the same bytes for a foreign and a local Uint8Array", async () => {
      const code = "Uint8Array.from([1, 2, 3, 4, 5, 255])";
      const local = await upload(Uint8Array.from([1, 2, 3, 4, 5, 255]));
      const remote = await upload(foreign<Uint8Array>(code));
      expect([...remote.bytes]).toEqual([1, 2, 3, 4, 5, 255]);
      expect([...remote.bytes]).toEqual([...local.bytes]);
      expect(remote.finish.sizeBytes).toBe(6n);
    });

    test("keeps the byteOffset and length of a foreign typed-array slice", async () => {
      const code =
        "const b = Uint8Array.from([9, 9, 1, 2, 3, 9, 9]); b.subarray(2, 5)";
      const slice = foreign<Uint8Array>(code);
      expect(slice.byteOffset).toBe(2);
      expect(slice.buffer.byteLength).toBe(7);
      const local = await upload(
        Uint8Array.from([9, 9, 1, 2, 3, 9, 9]).subarray(2, 5),
      );
      const remote = await upload(slice);
      expect([...remote.bytes]).toEqual([1, 2, 3]);
      expect([...remote.bytes]).toEqual([...local.bytes]);
      expect(remote.finish.sizeBytes).toBe(3n);
    });

    test("uploads the same bytes for a foreign and a local ArrayBuffer", async () => {
      const code = "Uint8Array.from([7, 8, 9]).buffer";
      const local = await upload(Uint8Array.from([7, 8, 9]).buffer);
      const remote = await upload(foreign<ArrayBuffer>(code));
      expect([...remote.bytes]).toEqual([7, 8, 9]);
      expect([...remote.bytes]).toEqual([...local.bytes]);
    });

    test("chunks a large foreign Uint8Array at 1 MiB like a local one", async () => {
      const size = (2 << 20) + 3;
      const remoteSource = foreign<Uint8Array>(
        `const b = new Uint8Array(${size}); for (let i = 0; i < b.length; i++) b[i] = i % 251; b`,
      );
      const localSource = new Uint8Array(size);
      for (let i = 0; i < size; i++) localSource[i] = i % 251;
      const local = await upload(localSource);
      const remote = await upload(remoteSource);
      expect(remote.sizes).toEqual([1 << 20, 1 << 20, 3]);
      expect(remote.sizes).toEqual(local.sizes);
      expect(Buffer.from(remote.bytes).equals(Buffer.from(local.bytes))).toBe(
        true,
      );
    });

    test("an iterable of foreign pieces uploads the same bytes as local pieces", async () => {
      const pieces = foreign<Uint8Array[]>(
        "[Uint8Array.from([1, 2]), Uint8Array.from([3, 4, 5, 6]).subarray(1, 3)]",
      );
      const remote = await upload(pieces);
      const buffer = foreign<unknown>("Uint8Array.from([8, 9]).buffer");
      const remoteBuffer = await upload([buffer] as any);
      expect([...remote.bytes]).toEqual([1, 2, 4, 5]);
      expect([...remoteBuffer.bytes]).toEqual([8, 9]);
    });

    test("an object that only fakes the type tag is not read as a binary source", async () => {
      const h = setup();
      const fake = { [Symbol.toStringTag]: "Uint8Array", length: 2 };
      await expect(
        h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", fake as any)),
      ).rejects.toBeDefined();
      expect(h.api.callsOf("WriteFile")).toHaveLength(0);
    });

    test("existing local sources keep their chunking", async () => {
      const text = await upload("héllo");
      expect(Buffer.from(text.bytes).toString()).toBe("héllo");
      const stream = await upload(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(Uint8Array.from([1, 2]));
            controller.enqueue(Uint8Array.from([3]));
            controller.close();
          },
        }),
      );
      expect([...stream.bytes]).toEqual([1, 2, 3]);
      expect(stream.sizes).toEqual([3]);
    });
  });

  describe("write_id allocation while the source is slow", () => {
    // The agent admits an operation ID only while its UUID v7 timestamp is at
    // most 15 minutes old, so a write_id that is allocated before a slow
    // source delivers its first sendable chunk is rejected as expired.
    const ID_WINDOW_MS = 15 * MIN;

    function admittingHandler(h: ReturnType<typeof setup>) {
      h.api.unary(RelayService, "WriteFile", (req) => {
        const age = h.clock.wallNow() - uuidV7TimestampMs(req.writeId);
        if (age >= ID_WINDOW_MS) {
          throw ownsandError(Code.NotFound, "the operation ID has expired");
        }
        return {
          writeId: req.writeId,
          offset: req.offset,
          length: BigInt(req.data.length),
          finished: req.finish !== undefined,
          sizeBytes: req.finish?.sizeBytes ?? 0n,
        };
      });
    }

    function expectFreshFirstRequest(
      h: ReturnType<typeof setup>,
      bytes: number,
    ) {
      const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
      const auths = h.api
        .callsOf("AuthorizeRelay")
        .map((c) => c.request as any);
      expect(writes).toHaveLength(1);
      expect(writes[0].offset).toBe(0n);
      expect(writes[0].data).toHaveLength(bytes);
      expect(writes[0].finish.sizeBytes).toBe(BigInt(bytes));
      expect(
        h.clock.wallNow() - uuidV7TimestampMs(writes[0].writeId),
      ).toBeLessThan(1000);
      expect(auths).toHaveLength(1);
      expect(auths[0].operationKind).toBe(RelayOperationKind.WRITE_FILE);
      expect(auths[0].operationId).toBe(writes[0].writeId);
      expect(Buffer.from(auths[0].requestDigest)).toEqual(
        Buffer.from(requestDigest(writeFileBinding(writes[0]))),
      );
    }

    test("an async iterable that waits 15 minutes for its first byte still gets a fresh write_id", async () => {
      const h = setup();
      admittingHandler(h);
      async function* source() {
        await h.clock.sleep(ID_WINDOW_MS);
        yield new TextEncoder().encode("hello");
      }
      const result = await h.clock.run(
        h.client.sandbox(SANDBOX).writeFile("/f", source()),
      );
      expectFreshFirstRequest(h, 5);
      expect(result.sizeBytes).toBe(5n);
      const [write] = h.api.callsOf("WriteFile").map((c) => c.request as any);
      expect(result.writeId).toBe(write.writeId);
    });

    test("a source that waits 15 minutes for its end after a full chunk still gets a fresh write_id", async () => {
      const h = setup();
      admittingHandler(h);
      async function* source() {
        yield new Uint8Array(1 << 20);
        await h.clock.sleep(ID_WINDOW_MS);
      }
      await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", source()));
      expectFreshFirstRequest(h, 1 << 20);
    });

    test("a ReadableStream that closes after 15 minutes still gets a fresh write_id", async () => {
      const h = setup();
      admittingHandler(h);
      const stream = new ReadableStream<Uint8Array>({
        async pull(controller) {
          await h.clock.sleep(ID_WINDOW_MS);
          controller.enqueue(new TextEncoder().encode("hello"));
          controller.close();
        },
      });
      await h.clock.run(h.client.sandbox(SANDBOX).writeFile("/f", stream));
      expectFreshFirstRequest(h, 5);
    });

    test("a retry of the first unacknowledged request keeps its identity and later chunks keep the write_id", async () => {
      const h = setup();
      h.api.unary(RelayService, "WriteFile", (req, call) => {
        if (call.index === 0) {
          throw new ConnectError("relay call limit", Code.DeadlineExceeded);
        }
        return {
          writeId: req.writeId,
          offset: req.offset,
          length: BigInt(req.data.length),
          finished: req.finish !== undefined,
          sizeBytes: req.finish?.sizeBytes ?? 0n,
        };
      });
      async function* source() {
        await h.clock.sleep(ID_WINDOW_MS);
        yield new Uint8Array(1 << 20);
        yield new Uint8Array(1 << 20);
        yield new Uint8Array(10);
      }
      const result = await h.clock.run(
        h.client.sandbox(SANDBOX).writeFile("/f", source()),
      );
      const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
      expect(writes).toHaveLength(4);
      expect(
        h.clock.wallNow() - uuidV7TimestampMs(writes[0].writeId),
      ).toBeLessThan(MIN);
      expect(writes[1].writeId).toBe(writes[0].writeId);
      expect(writes[1].path).toBe("/f");
      expect(writes[1].path).toBe(writes[0].path);
      expect(writes[0].bindingSalt).toHaveLength(32);
      expect(Buffer.from(writes[1].bindingSalt)).toEqual(
        Buffer.from(writes[0].bindingSalt),
      );
      expect(writes[1].offset).toBe(writes[0].offset);
      expect(Buffer.from(writes[1].data)).toEqual(Buffer.from(writes[0].data));
      expect(writes[1].finish).toEqual(writes[0].finish);
      expect(writes.map((w) => w.offset)).toEqual([
        0n,
        0n,
        BigInt(1 << 20),
        BigInt(2 << 20),
      ]);
      for (const write of writes) {
        expect(write.writeId).toBe(writes[0].writeId);
      }
      expect(writes[3].finish.sizeBytes).toBe(BigInt((2 << 20) + 10));
      expect(result.writeId).toBe(writes[0].writeId);
      for (const auth of h.api.callsOf("AuthorizeRelay")) {
        expect((auth.request as any).operationId).toBe(writes[0].writeId);
      }
    });

    test("an abort while the source waits rejects with the reason and sends no request", async () => {
      const h = setup();
      admittingHandler(h);
      const REASON = new Error("caller gave up");
      const controller = new AbortController();
      async function* source() {
        await h.clock.sleep(ID_WINDOW_MS);
        yield new Uint8Array(1);
      }
      const outcome = h.client
        .sandbox(SANDBOX)
        .writeFile("/f", source(), { signal: controller.signal })
        .then(
          () => "resolved",
          (error) => error,
        );
      await h.clock.advance(5 * MIN);
      controller.abort(REASON);
      expect(await outcome).toBe(REASON);
      expect(h.api.callsOf("WriteFile")).toHaveLength(0);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });
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

  test.each([-1n, -(2n ** 62n)])(
    "rejects readFile offset %j synchronously before any request",
    (offset) => {
      const h = setup();
      let error: unknown;
      try {
        h.client.sandbox(SANDBOX).readFile("/f", { offset });
      } catch (e) {
        error = e;
      }
      expect(error).toBeInstanceOf(OwnSandValidationError);
      expect((error as OwnSandValidationError).field).toBe("offset");
      expect((error as OwnSandValidationError).message).toContain(
        "must not be negative",
      );
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test("rejects readFileBytes with a negative offset before any request", async () => {
    const h = setup();
    await expect(
      h.client.sandbox(SANDBOX).readFileBytes("/f", { offset: -1n } as never),
    ).rejects.toBeInstanceOf(OwnSandValidationError);
    expect(h.api.calls).toHaveLength(0);
  });

  test("reports the path before a negative offset", () => {
    const h = setup();
    let error: unknown;
    try {
      h.client.sandbox(SANDBOX).readFile("relative", { offset: -1n });
    } catch (e) {
      error = e;
    }
    expect((error as OwnSandValidationError).field).toBe("path");
    expect(h.api.calls).toHaveLength(0);
  });

  test.each([0o10000, -1, 1.5, Number.NaN, Number.POSITIVE_INFINITY])(
    "rejects write mode %j with finish.mode before any request",
    async (mode) => {
      const h = setup();
      const error = await h.client
        .sandbox(SANDBOX)
        .writeFile("/out", "x", { mode })
        .catch((e: unknown) => e);
      expect(error).toBeInstanceOf(OwnSandValidationError);
      expect((error as OwnSandValidationError).field).toBe("finish.mode");
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test.each([
    [undefined, 0o644],
    [0, 0],
    [0o7777, 0o7777],
  ])("sends write mode %j as finish.mode %j", async (mode, want) => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", (req) => ({
      writeId: req.writeId,
      offset: req.offset,
      length: BigInt(req.data.length),
      finished: req.finish !== undefined,
      sizeBytes: req.finish?.sizeBytes ?? 0n,
    }));
    await h.clock.run(
      h.client.sandbox(SANDBOX).writeFile("/out", "x", { mode }),
    );
    const writes = h.api.callsOf("WriteFile").map((c) => c.request as any);
    expect(writes[writes.length - 1].finish.mode).toBe(want);
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
    // Retain the yielded views themselves and read them only after the loop:
    // a chunk that aliased the input would show the overwritten bytes.
    const views: Uint8Array[] = [];
    for await (const c of chunkSource(input, 4)) {
      views.push(c.data);
      input.fill(9);
    }
    expect(views.map((view) => [...view])).toEqual([
      [1, 1, 1, 1],
      [2, 2, 2, 2],
      [3],
    ]);
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
    const NativeUint8Array = globalThis.Uint8Array;
    let copied = 0;
    let constructed = 0;
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
    // `new Uint8Array(typedArrayOrArrayLike)` copies its argument without
    // calling `slice` or `set`, so count those constructor calls too. A
    // numeric length allocates without copying and an `ArrayBuffer` argument
    // creates a view, so neither counts. `subarray` builds its views through
    // the original constructor, which this proxy does not see.
    globalThis.Uint8Array = new Proxy(NativeUint8Array, {
      construct(target, args, newTarget) {
        const first: unknown = args[0];
        if (
          typeof first === "object" &&
          first !== null &&
          !(first instanceof ArrayBuffer) &&
          !(first instanceof SharedArrayBuffer)
        ) {
          const length = (first as ArrayLike<number>).length;
          copied += length;
          constructed += length;
        }
        return Reflect.construct(
          target,
          args,
          newTarget === globalThis.Uint8Array ? target : newTarget,
        );
      },
    });

    try {
      for (const source of [input, split()]) {
        copied = 0;
        constructed = 0;
        const chunks: Uint8Array[] = [];
        let lasts = 0;
        for await (const c of chunkSource(source, size)) {
          chunks.push(c.data);
          if (c.last) lasts++;
        }
        // The ownership copy in `chunkSource` copies every byte once and
        // `take` copies it once more, so the volume is about twice the input.
        // An extra copy on either path pushes it above the bound.
        expect(constructed).toBeGreaterThan(0);
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
      globalThis.Uint8Array = NativeUint8Array;
      Uint8Array.prototype.slice = slice;
      Uint8Array.prototype.set = set;
    }
  });
});
