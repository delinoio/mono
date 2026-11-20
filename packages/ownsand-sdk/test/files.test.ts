// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";

import { DataPathError } from "../src/errors.ts";
import { RelayOperationKind } from "../src/gen/delino/ownsand/v1/common_pb.ts";
import { rechunk, WRITE_CHUNK_BYTES } from "../src/relay/write.ts";
import { exhausted } from "./support/fake.ts";
import { harness, rejection, text } from "./support/harness.ts";

const MIN = 60_000;
const MiB = 1024 * 1024;

function bytes(n: number, seed = 1): Uint8Array {
  const b = new Uint8Array(n);
  for (let i = 0; i < n; i++) {
    b[i] = (i * 31 + seed) % 251;
  }
  return b;
}

const same = (a: Uint8Array, b: Uint8Array) =>
  Buffer.from(a).equals(Buffer.from(b));

describe("readFile over the relay", () => {
  test("streams chunks and uses a new read_id and salt for each read", async () => {
    const h = harness();
    const data = bytes(300 * 1024);
    h.fake.files.set("/data.bin", data);
    expect(
      same(await h.run(h.sandbox.readFile("/data.bin").bytes()), data),
    ).toBe(true);
    expect(
      same(await h.run(h.sandbox.readFile("/data.bin").bytes()), data),
    ).toBe(true);
    const reads = h.fake.callsOf("ReadFile");
    expect(reads).toHaveLength(2);
    expect(reads[0]!.operationId).not.toBe(reads[1]!.operationId);
    expect(same(reads[0]!.salt!, reads[1]!.salt!)).toBe(false);
    expect(h.fake.callsOf("AuthorizeRelay").map((c) => c.kind)).toEqual([
      RelayOperationKind.READ_FILE,
      RelayOperationKind.READ_FILE,
    ]);
  });

  test("an abrupt relay disconnect after a delivered chunk resumes at the next offset", async () => {
    const h = harness();
    const data = bytes(300 * 1024);
    h.fake.files.set("/data.bin", data);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectAfterData: 2 } : undefined;
    expect(
      same(await h.run(h.sandbox.readFile("/data.bin").bytes()), data),
    ).toBe(true);
    const reads = h.fake.callsOf("ReadFile");
    expect(reads.map((r) => r.offset)).toEqual([0n, 2n * 64n * 1024n]);
    expect(reads[1]!.operationId).toBe(reads[0]!.operationId);
    expect(same(reads[1]!.salt!, reads[0]!.salt!)).toBe(true);
    expect(h.fake.violations).toEqual([]);
  });

  test("repeated accepted continuations that end without data at the same offset end the operation within the window", async () => {
    const h = harness();
    h.fake.files.set("/data.bin", bytes(300 * 1024));
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : { acceptDelayMs: 15_000, stallAfterAccept: true };
    const err = await h.run(rejection(h.sandbox.readFile("/data.bin").bytes()));
    expect(err.message).toMatch(/90 seconds/);
    const reads = h.fake.callsOf("ReadFile");
    expect(new Set(reads.slice(1).map((r) => r.offset))).toEqual(
      new Set([64n * 1024n]),
    );
    // Accepted stopped the clock for 90 s each time, but the 15 s before
    // accepted counted, so the window ended after a few continuations.
    expect(reads.length).toBeGreaterThanOrEqual(4);
    expect(reads.length).toBeLessThanOrEqual(6);
    expect(h.clock.now()).toBeGreaterThan(5 * MIN);
  });

  test("a recovery read whose first chunk takes longer than 90 seconds to arrive while bytes keep arriving", async () => {
    const h = harness();
    h.fake.readChunkBytes = MiB;
    const data = bytes(MiB + 1000);
    h.fake.files.set("/big.bin", data);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectBeforeAccept: true }
        : n === 1
          ? { drip: { pieces: 8, intervalMs: 20_000 } }
          : undefined;
    expect(
      same(await h.run(h.sandbox.readFile("/big.bin").bytes()), data),
    ).toBe(true);
    expect(h.fake.callsOf("ReadFile")).toHaveLength(2);
    expect(h.clock.now()).toBeGreaterThan(2 * MIN);
  });

  test("an expired read_id is a visible error", async () => {
    const h = harness();
    h.fake.files.set("/data.bin", bytes(300 * 1024));
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : { reject: new ConnectError("read expired", Code.NotFound) };
    const err = await h.run(rejection(h.sandbox.readFile("/data.bin").bytes()));
    expect(err).toBeInstanceOf(DataPathError);
    expect((err as DataPathError).code).toBe(Code.NotFound);
    expect(h.fake.callsOf("ReadFile")).toHaveLength(2);
  });

  test("an explicit aborted status from the guest helper is terminal", async () => {
    const h = harness();
    h.fake.files.set("/data.bin", bytes(10));
    h.fake.hooks.stream = () => ({
      reject: new ConnectError("file changed during the read", Code.Aborted),
    });
    const err = (await h.run(
      rejection(h.sandbox.readFile("/data.bin").bytes()),
    )) as ConnectError;
    expect(err.code).toBe(Code.Aborted);
    expect(h.fake.callsOf("ReadFile")).toHaveLength(1);
  });

  test("rejects a relative path before sending", () => {
    const h = harness();
    expect(() => h.sandbox.readFile("relative.txt")).toThrow(/absolute/);
  });
});

describe("writeFile over the relay", () => {
  test("splits any input into chunks of at most 1 MiB without holding back reader data", async () => {
    const parts: Uint8Array[] = [];
    async function* reader() {
      yield bytes(700 * 1024);
      yield "text";
      yield bytes(700 * 1024, 2);
    }
    for await (const c of rechunk(reader())) {
      parts.push(c);
    }
    expect(parts.map((p) => p.byteLength)).toEqual([700 * 1024, 4, 700 * 1024]);
    const whole: Uint8Array[] = [];
    for await (const c of rechunk(bytes(2 * WRITE_CHUNK_BYTES + 3))) {
      whole.push(c);
    }
    expect(whole.map((p) => p.byteLength)).toEqual([
      WRITE_CHUNK_BYTES,
      WRITE_CHUNK_BYTES,
      3,
    ]);
  });

  test("chunked file writes reuse one grant and send path and salt only with the first chunk", async () => {
    const h = harness();
    const data = bytes(2 * MiB + 500);
    const res = await h.run(
      h.sandbox.writeFile("/out.bin", data, { mode: 0o600 }),
    );
    expect(res.sizeBytes).toBe(BigInt(data.byteLength));
    expect(same(h.fake.files.get("/out.bin")!, data)).toBe(true);
    const writes = h.fake.callsOf("WriteFile");
    expect(writes.map((w) => [w.writeOffset, w.finish])).toEqual([
      [0n, false],
      [BigInt(MiB), false],
      [BigInt(2 * MiB), false],
      [undefined, true],
    ]);
    expect(writes.map((w) => w.path)).toEqual(["/out.bin", "", "", ""]);
    expect(writes.slice(1).every((w) => w.salt!.byteLength === 0)).toBe(true);
    expect(new Set(writes.map((w) => w.grantId)).size).toBe(1);
    expect(h.fake.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("writes an empty file with a finish request that carries the path", async () => {
    const h = harness();
    await h.run(h.sandbox.writeFile("/empty", new Uint8Array()));
    expect(h.fake.files.get("/empty")!.byteLength).toBe(0);
    expect(h.fake.callsOf("WriteFile")).toHaveLength(1);
  });

  test("after a lost response, sends the exact unacknowledged request again", async () => {
    const h = harness();
    const data = bytes(MiB + 10);
    h.fake.hooks.write = (_req, n) =>
      n === 1 ? { disconnectAfterApply: true } : undefined;
    await h.run(h.sandbox.writeFile("/out.bin", data));
    expect(h.fake.callsOf("WriteFile").map((w) => w.writeOffset)).toEqual([
      0n,
      BigInt(MiB),
      BigInt(MiB),
      undefined,
    ]);
    expect(same(h.fake.files.get("/out.bin")!, data)).toBe(true);
  });

  test("retries a chunk after deadline_exceeded with the same write_id", async () => {
    const h = harness();
    h.fake.hooks.write = (_req, n) =>
      n === 0 ? { deadlineAfterMs: 5 * MIN } : undefined;
    await h.run(h.sandbox.writeFile("/out.bin", "hello"));
    const writes = h.fake.callsOf("WriteFile");
    expect(writes.map((w) => w.writeOffset)).toEqual([0n, 0n, undefined]);
    expect(new Set(writes.map((w) => w.operationId)).size).toBe(1);
    expect(text(h.fake.files.get("/out.bin")!)).toBe("hello");
  });

  test("cancels an attempt that gets no response before its SDK-side deadline and retries it", async () => {
    const h = harness();
    h.fake.hooks.write = (_req, n) => (n === 0 ? { hang: true } : undefined);
    await h.run(h.sandbox.writeFile("/out.bin", "hello"));
    const writes = h.fake.callsOf("WriteFile");
    expect(writes).toHaveLength(3);
    expect(writes[1]!.at - writes[0]!.at).toBeGreaterThanOrEqual(
      5 * MIN + 30_000,
    );
    expect(writes[1]!.at - writes[0]!.at).toBeLessThan(5 * MIN + 45_000);
    // The cancelled attempt's stream closed before the renewal: no violation.
    expect(h.fake.violations).toEqual([]);
  });

  test("a chunk rejected at agent capacity is retried; an unacknowledged chunk has a 60-second budget", async () => {
    const h = harness();
    h.fake.hooks.write = (_req, n) =>
      n < 2 ? { reject: exhausted() } : undefined;
    await h.run(h.sandbox.writeFile("/out.bin", "x"));

    const h2 = harness();
    h2.fake.hooks.write = () => ({
      reject: new ConnectError("no agent relay connection", Code.Unavailable),
    });
    const err = (await h2.run(
      rejection(h2.sandbox.writeFile("/out.bin", "x")),
    )) as ConnectError;
    expect(err.code).toBe(Code.Unavailable);
    expect(h2.clock.now()).toBeLessThanOrEqual(60_000);
    expect(h2.fake.callsOf("WriteFile").length).toBeGreaterThan(4);
  });

  test("renews before a chunk that follows a three-minute pause on a device with one grant", async () => {
    const h = harness();
    h.fake.grantLimit = 1;
    const clock = h.clock;
    async function* slowReader() {
      yield bytes(MiB);
      await clock.sleep(3 * MIN);
      yield bytes(100, 7);
    }
    await h.run(h.sandbox.writeFile("/out.bin", slowReader()));
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    expect(auth[1]!.replaces).toBe(h.fake.callsOf("WriteFile")[0]!.grantId);
    expect(auth[1]!.at).toBeGreaterThanOrEqual(3 * MIN);
    expect(h.fake.violations).toEqual([]);
  });

  test("renews before a chunk whose grant would expire during the call", async () => {
    const h = harness();
    const clock = h.clock;
    async function* steady() {
      for (let i = 0; i < 15; i++) {
        yield bytes(1000, i);
        await clock.sleep(50_000);
      }
    }
    await h.run(h.sandbox.writeFile("/out.bin", steady()));
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    // 15 minutes of grant lifetime minus the 5.5-minute call need.
    expect(auth[1]!.at).toBeGreaterThan(9.5 * MIN);
    expect(auth[1]!.at).toBeLessThanOrEqual(9.5 * MIN + 50_000);
    expect(auth[1]!.replaces).toBeDefined();
  });

  test("measures renewal from expires_in, not from expires_at, with a caller clock offset", async () => {
    for (const skew of [2 * 60 * MIN, -2 * 60 * MIN]) {
      const h = harness();
      h.clock.wallSkewMs = skew;
      const clock = h.clock;
      async function* steady() {
        for (let i = 0; i < 6; i++) {
          yield bytes(1000, i);
          await clock.sleep(50_000);
        }
      }
      await h.run(h.sandbox.writeFile("/out.bin", steady()));
      expect(h.fake.callsOf("AuthorizeRelay")).toHaveLength(1);
    }
  });

  test("renewal outside recovery has its own 60-second retry window", async () => {
    const h = harness();
    const clock = h.clock;
    async function* paused() {
      yield "a";
      await clock.sleep(2 * MIN);
      yield "b";
    }
    h.fake.hooks.authorize = (_req, n) =>
      n >= 1 && n <= 3
        ? {
            reject: new ConnectError(
              "agent not acknowledging",
              Code.Unavailable,
            ),
          }
        : undefined;
    await h.run(h.sandbox.writeFile("/out.bin", paused()));
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(5);
    // Every retry of the renewal keeps its authorization_id and replaces_grant_id.
    expect(new Set(auth.slice(1).map((c) => c.authorizationId)).size).toBe(1);
    expect(new Set(auth.slice(1).map((c) => c.replaces)).size).toBe(1);

    const h2 = harness();
    const clock2 = h2.clock;
    async function* paused2() {
      yield "a";
      await clock2.sleep(2 * MIN);
      yield "b";
    }
    h2.fake.hooks.authorize = (_req, n) =>
      n >= 1
        ? {
            reject: new ConnectError(
              "agent not acknowledging",
              Code.Unavailable,
            ),
          }
        : undefined;
    const err = (await h2.run(
      rejection(h2.sandbox.writeFile("/out.bin", paused2())),
    )) as ConnectError;
    expect(err.code).toBe(Code.Unavailable);
    expect(h2.clock.now()).toBeLessThanOrEqual(3 * MIN + 1_000);
    // No chunk was sent with the stale grant.
    expect(h2.fake.callsOf("WriteFile")).toHaveLength(1);
  });

  test("a lost renewal response after the agent installed the replacement grant", async () => {
    const h = harness();
    const clock = h.clock;
    async function* paused() {
      yield "a";
      await clock.sleep(2 * MIN);
      yield "b";
    }
    h.fake.hooks.authorize = (_req, n) =>
      n === 1 ? { loseResponse: true } : undefined;
    await h.run(h.sandbox.writeFile("/out.bin", paused()));
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(3);
    expect(auth[2]!.authorizationId).toBe(auth[1]!.authorizationId);
    // The original grant plus one replacement; the retry created no third grant.
    expect(h.fake.grants.size).toBe(2);
    const writes = h.fake.callsOf("WriteFile");
    expect(writes[1]!.grantId).toBe([...h.fake.grants.keys()][1]);
    expect(text(h.fake.files.get("/out.bin")!)).toBe("ab");
  });
});
