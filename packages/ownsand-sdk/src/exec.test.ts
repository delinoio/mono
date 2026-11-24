// SPDX-License-Identifier: Apache-2.0
import { getEventListeners } from "node:events";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import type { Clock } from "./clock.ts";
import { execBinding, requestDigest } from "./digest.ts";
import { DataPathError, OwnSandValidationError } from "./errors.ts";
import { Timing } from "./relay/context.ts";
import { AcceptanceDeadlineError } from "./relay/stream.ts";
import {
  BillingReason,
  DataPathTransport,
  RelayOperationKind,
  SandboxEndReason,
  SandboxState,
  SessionEndReason,
} from "@delino/ownsand-protos/v1/common_pb";
import { DataPathService } from "@delino/ownsand-protos/v1/data_path_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import type { DataPathSelector } from "./datapath.ts";
import { billingReasonOf } from "./errors.ts";
import {
  ExecEventType,
  ExecHandle,
  type ExecResult,
  ExecResultKind,
  type ExecStreamEvent,
} from "./operations/exec.ts";
import type { VirtualClock } from "./testing/virtual-clock.ts";
import {
  Disconnect,
  Eof,
  Hang,
  LostResponse,
  ownsandError,
  Sleep,
} from "./testing/fake-api.ts";
import {
  API_KEY,
  attachMsg,
  ev,
  execMsg,
  ORG,
  SANDBOX,
  setup,
  text,
} from "./testing/harness.ts";
import {
  expectTransportInterruption,
  expectUnacceptedStreamEnd,
  expectWindowExhausted,
} from "./testing/recovery-assert.ts";

const MIN = 60_000;

async function collect(handle: AsyncIterable<ExecStreamEvent>) {
  const events: ExecStreamEvent[] = [];
  for await (const e of handle) events.push(e);
  return events;
}

function stdoutText(events: ExecStreamEvent[]): string {
  return events
    .map((e) => (e.type === ExecEventType.Stdout ? text(e.data) : ""))
    .join("");
}

/**
 * An exec generator that yields one event, then waits for `gate` and ends
 * with `outcome`. It stands in for the relay generator, whose reads queue in
 * order and which closes after a rejection.
 */
function gatedGenerator(
  gate: Promise<void>,
  outcome: { reject: unknown } | { result: ExecResult },
): AsyncGenerator<ExecStreamEvent, ExecResult> {
  return (async function* () {
    yield { type: ExecEventType.Started, execId: "e" } as ExecStreamEvent;
    await gate;
    if ("reject" in outcome) throw outcome.reject;
    return outcome.result;
  })();
}

function gatedHandle(
  gate: Promise<void>,
  outcome: { reject: unknown } | { result: ExecResult },
): ExecHandle {
  return new ExecHandle(
    "e",
    gatedGenerator(gate, outcome),
    {} as DataPathSelector,
  );
}

function openGate() {
  let open!: () => void;
  const gate = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { gate, open };
}

/** Lets queued microtasks run, so every read is queued before the gate opens. */
const settle = () => new Promise<void>((resolve) => setImmediate(resolve));

describe("exec handle after a terminal failure", () => {
  const failure = new DataPathError("the exec stream failed", {
    code: Code.ResourceExhausted,
  });

  test("an iterator that is queued before wait() and a later wait() both see the rejection", async () => {
    const { gate, open } = openGate();
    const handle = gatedHandle(gate, { reject: failure });
    const iterator = handle[Symbol.asyncIterator]();
    expect((await iterator.next()).done).toBe(false);
    const nextEvent = iterator.next().catch((e: unknown) => e);
    const waited = handle.wait().catch((e: unknown) => e);
    await settle();
    open();
    expect(await nextEvent).toBe(failure);
    expect(await waited).toBe(failure);
    expect(await handle.wait().catch((e: unknown) => e)).toBe(failure);
    expect(await handle.wait().catch((e: unknown) => e)).toBe(failure);
  });

  test("wait() owns the failing read and the iterator, which queued behind it, still sees the rejection", async () => {
    const { gate, open } = openGate();
    const handle = gatedHandle(gate, { reject: failure });
    const iterator = handle[Symbol.asyncIterator]();
    expect((await iterator.next()).done).toBe(false);
    // wait() reads first; the iterator's next read queues behind it.
    const waited = handle.wait().catch((e: unknown) => e);
    const nextEvent = iterator.next().catch((e: unknown) => e);
    await settle();
    open();
    expect(await waited).toBe(failure);
    expect(await nextEvent).toBe(failure);
    expect(await handle.wait().catch((e: unknown) => e)).toBe(failure);
  });

  test("overlapping wait() calls share the rejection", async () => {
    const { gate, open } = openGate();
    const handle = gatedHandle(gate, { reject: failure });
    const first = handle.wait().catch((e: unknown) => e);
    const second = handle.wait().catch((e: unknown) => e);
    await settle();
    open();
    expect(await first).toBe(failure);
    expect(await second).toBe(failure);
  });

  test("records the abort reason before closing the generator at its cached event", async () => {
    const cleanup = openGate();
    let closing = false;
    let advanced = false;
    async function* stream(): AsyncGenerator<ExecStreamEvent, ExecResult> {
      try {
        yield { type: ExecEventType.Started, execId: "e" };
        advanced = true;
        throw new Error("unexpected stream read");
      } finally {
        closing = true;
        await cleanup.gate;
      }
    }

    const caller = new AbortController();
    const handle = new ExecHandle(
      "e",
      stream(),
      {} as DataPathSelector,
      undefined,
      true,
      caller.signal,
    );
    await settle();
    caller.abort(null);
    const first = expect(handle[Symbol.asyncIterator]().next()).rejects.toBe(
      null,
    );
    let overlapping: unknown = undefined;
    const waited = handle.wait().catch((error: unknown) => {
      overlapping = error;
    });
    try {
      await settle();
      expect(closing).toBe(true);
      expect(advanced).toBe(false);
      expect(overlapping).toBe(null);
    } finally {
      cleanup.open();
      await Promise.all([first, waited]);
    }
  });

  test("a rejection that is not an Error, including undefined, is reported as it is", async () => {
    for (const reason of ["aborted by the caller", undefined, 0, null]) {
      const { gate, open } = openGate();
      const handle = gatedHandle(gate, { reject: reason });
      const iterator = handle[Symbol.asyncIterator]();
      await iterator.next();
      const nextEvent = iterator.next().then(
        () => "resolved" as const,
        (e: unknown) => ({ rejected: e }),
      );
      const waited = handle.wait().then(
        () => "resolved" as const,
        (e: unknown) => ({ rejected: e }),
      );
      await settle();
      open();
      expect(await nextEvent).toEqual({ rejected: reason });
      expect(await waited).toEqual({ rejected: reason });
      expect(
        await handle.wait().then(
          () => "resolved" as const,
          (e: unknown) => ({ rejected: e }),
        ),
      ).toEqual({ rejected: reason });
    }
  });

  test("a success is unchanged: both readers finish and wait() keeps the result", async () => {
    const { gate, open } = openGate();
    const result: ExecResult = {
      kind: ExecResultKind.Exited,
      exitCode: 0n,
      signal: undefined,
      timedOut: false,
      complete: true,
    };
    const handle = gatedHandle(gate, { result });
    const events = collect(handle);
    const waited = handle.wait();
    await settle();
    open();
    await events;
    expect(await waited).toBe(result);
    expect(await handle.wait()).toBe(result);
  });

  test("reports a billing suspension of a relay exec to the iterator and to every wait()", async () => {
    for (const order of ["iterator-first", "wait-first"] as const) {
      const h = setup();
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.started());
        throw ownsandError(Code.ResourceExhausted, "suspended", {
          billingReason: BillingReason.SUSPENDED,
        });
      });
      const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
      const settled = (p: Promise<unknown>) => p.catch((e: unknown) => e);
      const run = async () => {
        const iterator = handle[Symbol.asyncIterator]();
        expect((await iterator.next()).done).toBe(false);
        // Iterator-first: the iterator owns the failing read and wait() queues
        // behind it. Wait-first: wait() owns the failing read and the
        // iterator queues behind it.
        const [first, second] =
          order === "iterator-first"
            ? [settled(iterator.next()), settled(handle.wait())]
            : [settled(handle.wait()), settled(iterator.next())];
        return { first: await first, second: await second };
      };
      const { first, second } = await h.clock.run(run());
      for (const error of [first, second, await settled(handle.wait())]) {
        expect(error).toBeInstanceOf(Error);
        expect(billingReasonOf(error)).toBe(BillingReason.SUSPENDED);
      }
      expect(second).toBe(first);
      expect(h.api.callsOf("Exec")).toHaveLength(1);
    }
  });
});

describe("exec over the relay", () => {
  for (const firstReader of ["iterator", "wait"] as const) {
    test(`rejects the first ${firstReader} read after cancellation with a cached event`, async () => {
      const h = setup();
      const seen: { signal?: AbortSignal } = {};
      h.api.stream(RelayService, "Exec", function* (req, call) {
        seen.signal = call.signal;
        yield execMsg(ev.accepted());
        yield execMsg(ev.started(req.execId));
        yield new Hang();
      });
      const caller = new AbortController();
      const reason = new Error("caller canceled after Started");
      const handle = h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"], signal: caller.signal });
      await h.clock.settle();
      expect(h.api.callsOf("Exec")).toHaveLength(1);
      expect(seen.signal?.aborted).toBe(false);

      caller.abort(reason);
      const first =
        firstReader === "iterator"
          ? handle[Symbol.asyncIterator]().next()
          : handle.wait();
      await h.clock.run(
        Promise.all([
          expect(first).rejects.toBe(reason),
          expect(handle.wait()).rejects.toBe(reason),
        ]),
      );
      await expect(handle.wait()).rejects.toBe(reason);
      expect(seen.signal?.aborted).toBe(true);
      expect(h.api.callsOf("Exec")).toHaveLength(1);
      expect(h.api.callsOf("AttachExec")).toHaveLength(0);
    });
  }

  test("delivers the cached Started once to a delayed active reader", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* (req) {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.exited(0));
    });
    const handle = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"], signal: new AbortController().signal });
    await h.clock.settle();
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(await h.clock.run(collect(handle))).toEqual([
      { type: ExecEventType.Started, execId: handle.execId },
    ]);
    expect(await handle.wait()).toMatchObject({
      kind: ExecResultKind.Exited,
      exitCode: 0n,
    });
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
  });

  test("runs a command relay-only without OpenSession and binds the grant to the request digest", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* (req) {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.stdout(0, "hello "));
      yield execMsg(ev.stderr(0, "warn"));
      yield execMsg(ev.stdout(6, "world"));
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const input = {
      command: ["/bin/echo", "hello 😀\ufffd"],
      env: { "key😀": "\ufffd", "key\ufffd": "😀" },
      workingDirectory: "/tmp/😀\ufffd",
    };
    const handle = sandbox.exec(input);
    const events = await h.clock.run(collect(handle));
    const result = await handle.wait();

    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    expect(h.api.callsOf("OpenSession")).toHaveLength(0);
    expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
    expect(events.map((e) => e.type)).toEqual([
      ExecEventType.Started,
      ExecEventType.Stdout,
      ExecEventType.Stderr,
      ExecEventType.Stdout,
    ]);
    expect(stdoutText(events)).toBe("hello world");
    expect(result).toEqual({
      kind: ExecResultKind.Exited,
      exitCode: 0n,
      signal: undefined,
      timedOut: false,
      complete: true,
    });

    const [auth] = h.api.callsOf("AuthorizeRelay");
    const [exec] = h.api.callsOf("Exec");
    const authReq = auth!.request as any;
    const execReq = exec!.request as any;
    expect(execReq).toMatchObject(input);
    expect(authReq.operationKind).toBe(RelayOperationKind.EXEC);
    expect(authReq.operationId).toBe(execReq.execId);
    expect(authReq.replacesGrantId).toBeUndefined();
    expect(Buffer.from(authReq.requestDigest)).toEqual(
      Buffer.from(requestDigest(execBinding(execReq))),
    );
    expect(execReq.bindingSalt).toHaveLength(32);
    expect(exec!.headers.get("ownsand-relay-grant-id")).toBe("grant-1");
    expect(exec!.headers.get("authorization")).toBe(`Bearer ${API_KEY}`);
    expect(exec!.headers.get("x-ownsand-sdk-activity")).toBeNull();
    expect(execReq.organizationId).toBe(ORG);
  });

  test("wait() drains the remaining events after a partial iteration and returns the same result each time", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* (req) {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.stdout(0, "a"));
      yield execMsg(ev.stdout(1, "b"));
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const handle = sandbox.exec({ command: ["sh", "-c", "echo a; echo b"] });
    const first = await h.clock.run(
      (async () => {
        for await (const e of handle) return e;
        return undefined;
      })(),
    );
    expect(first?.type).toBe(ExecEventType.Started);
    const result = await h.clock.run(handle.wait());
    expect(result).toMatchObject({
      kind: ExecResultKind.Exited,
      exitCode: 0n,
      complete: true,
    });
    expect(await handle.wait()).toBe(result);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
  });

  test("wait() without iterating returns the result and is repeatable", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["true"] });
    const result = await h.clock.run(handle.wait());
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(await handle.wait()).toBe(result);
  });

  test("overlapping wait() calls share one drain and keep the terminal result", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "a"));
      yield execMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["true"] });
    const [first, second] = await h.clock.run(
      Promise.all([handle.wait(), handle.wait()]),
    );
    expect(first.kind).toBe(ExecResultKind.Exited);
    expect(second).toBe(first);
    expect(await handle.wait()).toBe(first);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("wait() during an unfinished iteration keeps the result that iteration reaches", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "a"));
      yield execMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["true"] });
    const [events, result] = await h.clock.run(
      Promise.all([collect(handle), handle.wait()]),
    );
    expect(events.length).toBeLessThanOrEqual(2);
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(await handle.wait()).toBe(result);
  });

  test("reports a command that could not start, a timeout and a sandbox end as distinct results", async () => {
    const h = setup();
    let n = 0;
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      n++;
      if (n === 1) {
        yield execMsg({
          case: "failed",
          value: { reason: 1, message: "no such program" },
        });
      } else if (n === 2) {
        yield execMsg(ev.started());
        yield execMsg(ev.exited(143, true));
      } else {
        yield execMsg(ev.started());
        yield execMsg({
          case: "sandboxEnded",
          value: { reason: SandboxEndReason.MAX_TTL_EXPIRED },
        });
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const failed = await h.clock.run(
      sandbox.exec({ command: ["nope"] }).wait(),
    );
    const timedOut = await h.clock.run(
      sandbox.exec({ command: ["sleep", "100"], timeoutMs: 1500 }).wait(),
    );
    const ended = await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(failed.kind).toBe(ExecResultKind.Failed);
    expect(timedOut).toMatchObject({
      kind: ExecResultKind.Exited,
      exitCode: 143n,
      timedOut: true,
    });
    expect(ended).toMatchObject({
      kind: ExecResultKind.SandboxEnded,
      reason: SandboxEndReason.MAX_TTL_EXPIRED,
    });
    const timeout = (h.api.callsOf("Exec")[1]!.request as any).timeout;
    expect(timeout.seconds).toBe(1n);
    expect(timeout.nanos).toBe(500_000_000);
  });

  test("uses one grant per operation and a new salt for each operation", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["a"] }).wait());
    await h.clock.run(sandbox.exec({ command: ["a"] }).wait());
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    const execs = h.api.callsOf("Exec");
    expect(auths).toHaveLength(2);
    expect(auths[0].operationId).not.toBe(auths[1].operationId);
    expect(execs.map((c) => c.headers.get("ownsand-relay-grant-id"))).toEqual([
      "grant-1",
      "grant-2",
    ]);
    const salts = execs.map((c) =>
      Buffer.from((c.request as any).bindingSalt).toString("hex"),
    );
    expect(salts[0]).not.toBe(salts[1]);
  });

  test("rotates a silent exec without limit and renews the grant at each rotation 14 minutes after authorization", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      for (let i = 0; i < 13; i++) {
        yield new Sleep(MIN);
        yield execMsg(ev.keepalive());
      }
      yield new Sleep(MIN);
      // OK end without a final event: a rotation.
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.started());
      if (call.index < 3) {
        // A silent command: keepalives every minute, then the control plane
        // ends the stream at its 14-minute limit with OK.
        for (let i = 0; i < 13; i++) {
          yield new Sleep(MIN);
          yield attachMsg(ev.keepalive());
        }
        yield new Sleep(55_000);
        return;
      }
      yield attachMsg(ev.stdout(0, "done"));
      yield attachMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const handle = sandbox.exec({ command: ["sleep", "3600"] });
    const events = await h.clock.run(collect(handle));
    const result = await handle.wait();

    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.clock.now()).toBeGreaterThan(55 * MIN);
    expect(events.filter((e) => e.type === ExecEventType.Started)).toHaveLength(
      1,
    );
    expect(stdoutText(events)).toBe("done");
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(4);
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    // One authorization and one renewal per rotation, each replacing the
    // previous grant, all for the same operation with the same digest.
    expect(auths).toHaveLength(5);
    expect(auths.map((a) => a.replacesGrantId)).toEqual([
      undefined,
      "grant-1",
      "grant-2",
      "grant-3",
      "grant-4",
    ]);
    for (const a of auths) {
      expect(a.operationKind).toBe(RelayOperationKind.EXEC);
      expect(Buffer.from(a.requestDigest)).toEqual(
        Buffer.from(auths[0].requestDigest),
      );
    }
    const attach = h.api.callsOf("AttachExec").map((c) => c.request as any);
    expect(attach.every((a) => a.stdoutOffset === 0n)).toBe(true);
  });

  test("renews from expires_in on the monotonic clock even when the server wall clock is far off", async () => {
    const h = setup();
    h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => ({
      grantId: `grant-${call.index + 1}`,
      // A server clock 10 hours ahead of the caller's virtual wall clock.
      // The harness clock, not the host clock, is the caller's wall clock, so
      // the skew does not depend on the date the test runs on.
      expireTime: {
        seconds: BigInt(Math.floor(h.clock.wallNow() / 1000) + 36_000),
      },
      expiresIn: { seconds: 900n },
    }));
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      for (let i = 0; i < 5; i++) {
        yield new Sleep(MIN);
        yield execMsg(ev.keepalive());
      }
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    // At the rotation 5 minutes after authorization, the grant has 10
    // minutes left, which is less than the 14.5 minutes a new stream needs.
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
  });

  test("never runs the command again: continues with AttachExec from the delivered offsets after an interruption", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "abc"));
      yield execMsg(ev.stderr(0, "e"));
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (req) {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.started());
      // The agent repeats one byte that the caller already has.
      yield attachMsg(ev.stdout(Number(req.stdoutOffset) - 1, "cdef"));
      yield attachMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    const events = await h.clock.run(collect(handle));
    expect(stdoutText(events)).toBe("abcdef");
    expect(events.filter((e) => e.type === ExecEventType.Started)).toHaveLength(
      1,
    );
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    const attach = h.api.callsOf("AttachExec")[0]!.request as any;
    expect(attach.stdoutOffset).toBe(3n);
    expect(attach.stderrOffset).toBe(1n);
    // Recovery discards the grant and renews it with replaces_grant_id.
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(auths[1].replacesGrantId).toBe("grant-1");
    expect(
      h.api.callsOf("AttachExec")[0]!.headers.get("ownsand-relay-grant-id"),
    ).toBe("grant-2");
  });

  test("before started, tries AttachExec from zero first and resends the identical ExecRequest only on not_found", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* (_req, call) {
      if (call.index === 0) {
        yield new Disconnect();
        return;
      }
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      throw new ConnectError("unknown exec", Code.NotFound);
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    const execs = h.api.callsOf("Exec").map((c) => c.request as any);
    expect(execs).toHaveLength(2);
    expect(execs[1]).toEqual(execs[0]);
    const attach = h.api.callsOf("AttachExec")[0]!.request as any;
    expect(attach.stdoutOffset).toBe(0n);
    expect(attach.stderrOffset).toBe(0n);
  });

  test("ends with an unknown outcome when AttachExec returns not_found after started", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      throw new ConnectError("gone", Code.NotFound);
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error).toBeInstanceOf(DataPathError);
    expect(error.outcomeUnknown).toBe(true);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("hides accepted, keepalive and a repeated started after an AttachExec continuation", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.keepalive());
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.keepalive());
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.started());
      yield attachMsg(ev.exited(0));
    });
    const events = await h.clock.run(
      collect(h.client.sandbox(SANDBOX).exec({ command: ["x"] })),
    );
    expect(events.map((e) => e.type)).toEqual([ExecEventType.Started]);
  });

  test("reports a truncation with the missing offsets and marks the result incomplete", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "ab"));
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.started());
      yield attachMsg(ev.truncated(10, 0));
      yield attachMsg(ev.stdout(10, "kl"));
      yield attachMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    const events = await h.clock.run(collect(handle));
    const result = await handle.wait();
    expect(events.map((e) => e.type)).toEqual([
      ExecEventType.Started,
      ExecEventType.Stdout,
      ExecEventType.OutputTruncated,
      ExecEventType.Stdout,
    ]);
    expect(events[2]).toEqual({
      type: ExecEventType.OutputTruncated,
      stdout: { start: 2n, end: 10n },
      stderr: undefined,
    });
    expect(result.complete).toBe(false);
  });

  test("a stream that ends without accepted is a failure, not a rotation", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.keepalive());
      // OK end without `accepted`.
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.keepalive());
    });
    const started = h.clock.now();
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    // The operation ends with the failure of the last stream, not with a
    // helper error such as the virtual clock's deadlock.
    expectUnacceptedStreamEnd(error);
    // The Exec call and every AttachExec call fail; each failure but the
    // last one waits one back-off delay.
    const attaches = h.api.callsOf("AttachExec").length;
    expect(attaches).toBeGreaterThan(2);
    expectWindowExhausted(h.clock.now() - started, attaches);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("cancels an accepted stream that receives no byte for 90 seconds and continues", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "x"));
      yield new Hang();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.clock.now()).toBeGreaterThanOrEqual(90_000);
    expect(h.clock.now()).toBeLessThan(100_000);
    expect((h.api.callsOf("AttachExec")[0]!.request as any).stdoutOffset).toBe(
      1n,
    );
  });

  test("recovers from a second transport interruption within one recovery window", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      if (call.index === 0) {
        yield new Disconnect();
        return;
      }
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(3));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result).toMatchObject({ kind: ExecResultKind.Exited, exitCode: 3n });
    expect(h.api.callsOf("AttachExec")).toHaveLength(2);
  });

  test("a relay keepalive before accepted leaves the recovery timer running", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    let continuation: AbortSignal | undefined;
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      continuation = call.signal;
      // Keepalives every 30 seconds but never `accepted`, then a disconnect.
      for (let i = 0; i < 3; i++) {
        yield attachMsg(ev.keepalive());
        yield new Sleep(30_000);
      }
      yield new Disconnect();
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    // The recovery timer keeps running while only keepalives arrive, so the
    // window ends the continuation at 60 seconds, before its own 90-second
    // script (and its disconnect) could finish.
    expect(error).toBeInstanceOf(AcceptanceDeadlineError);
    expect(error.code).toBe(Code.Unavailable);
    expect(h.clock.now()).toBeGreaterThanOrEqual(MIN);
    expect(h.clock.now()).toBeLessThan(90_000);
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
    expect(continuation?.aborted).toBe(true);
  });

  describe("a recovery continuation that the agent does not accept", () => {
    const failFirstExec = (h: ReturnType<typeof setup>) =>
      h.api.stream(RelayService, "Exec", async function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.started());
        yield new Disconnect();
      });

    test("ends at the remaining window while only keepalives arrive", async () => {
      const h = setup();
      failFirstExec(h);
      let continuation: AbortSignal | undefined;
      h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
        continuation = call.signal;
        // A keepalive every 10 seconds, never `accepted`, never idle for 90.
        for (;;) {
          yield attachMsg(ev.keepalive());
          yield new Sleep(10_000);
        }
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(AcceptanceDeadlineError);
      expect(error.code).toBe(Code.Unavailable);
      // The window started at the failure at 0 s; the budget ends at 60 s.
      expect(h.clock.now()).toBeGreaterThanOrEqual(MIN);
      expect(h.clock.now()).toBeLessThan(MIN + 1_000);
      expect(h.api.callsOf("AttachExec")).toHaveLength(1);
      expect(continuation?.aborted).toBe(true);
    });

    test("ends at the remaining window while no frame arrives", async () => {
      const h = setup();
      failFirstExec(h);
      h.api.stream(RelayService, "AttachExec", async function* () {
        yield new Hang();
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(AcceptanceDeadlineError);
      // Before the fix, the 90-second idle watcher ended this stream.
      expect(h.clock.now()).toBeGreaterThanOrEqual(MIN);
      expect(h.clock.now()).toBeLessThan(MIN + 1_000);
      expect(h.api.callsOf("AttachExec")).toHaveLength(1);
    });

    test("keeps running past the window when accepted arrives in time", async () => {
      const h = setup();
      failFirstExec(h);
      h.api.stream(RelayService, "AttachExec", async function* () {
        yield new Sleep(30_000);
        yield attachMsg(ev.accepted());
        // Silent for longer than the window, with keepalives.
        for (let i = 0; i < 12; i++) {
          yield new Sleep(10_000);
          yield attachMsg(ev.keepalive());
        }
        yield attachMsg(ev.exited(0));
      });
      const result = await h.clock.run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      );
      expect(result.kind).toBe(ExecResultKind.Exited);
      expect(h.clock.now()).toBeGreaterThan(MIN + 30_000);
    });

    test("does not bound the first attempt outside recovery", async () => {
      const h = setup();
      h.api.stream(RelayService, "Exec", async function* () {
        for (let i = 0; i < 9; i++) {
          yield execMsg(ev.keepalive());
          yield new Sleep(10_000);
        }
        yield execMsg(ev.accepted());
        yield execMsg(ev.exited(0));
      });
      const result = await h.clock.run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      );
      expect(result.kind).toBe(ExecResultKind.Exited);
      expect(h.clock.now()).toBeGreaterThanOrEqual(90_000);
    });
  });

  test("an accepted continuation pauses the recovery clock and only data resets it", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      yield attachMsg(ev.accepted());
      if (call.index < 2) {
        // Accepted, open for 50 seconds with keepalives, then interrupted
        // without data.
        for (let i = 0; i < 5; i++) {
          yield new Sleep(10_000);
          yield attachMsg(ev.keepalive());
        }
        yield new Disconnect();
        return;
      }
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    // More than 100 seconds passed, but the open accepted continuations do
    // not count toward the 60-second window.
    expect(h.clock.now()).toBeGreaterThan(100_000);
    expect(result.kind).toBe(ExecResultKind.Exited);
  });

  test("two accepted continuations that disconnect at the same offset end the operation within the window", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield new Disconnect();
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    // The operation ends with the last transport interruption, not with a
    // helper error such as the virtual clock's deadlock.
    expectTransportInterruption(error);
    // The Exec call and every AttachExec call end in a disconnect; each
    // failure but the last one waits one back-off delay.
    const attaches = h.api.callsOf("AttachExec").length;
    expect(attaches).toBeGreaterThanOrEqual(2);
    expectWindowExhausted(h.clock.now(), attaches);
  });

  test("waits for a paused sandbox that resumes after 90 seconds before started", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* (_req, call) {
      if (call.index === 0) {
        yield new Disconnect();
        return;
      }
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      throw new ConnectError("no exec record", Code.NotFound);
    });
    h.state.value = SandboxState.PAUSED;
    setTimeoutVirtual(h, 90_000, () => {
      h.state.value = SandboxState.RUNNING;
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.clock.now()).toBeGreaterThanOrEqual(90_000);
    expect(h.api.callsOf("Exec")).toHaveLength(2);
  });

  test("an existing exec whose AttachExec returns paused resumes after more than 60 seconds", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "a"));
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      yield attachMsg(ev.accepted());
      if (call.index === 0) {
        yield attachMsg(ev.paused());
        return;
      }
      yield attachMsg(ev.stdout(1, "b"));
      yield attachMsg(ev.exited(0));
    });
    h.state.value = SandboxState.PAUSED;
    setTimeoutVirtual(h, 150_000, () => {
      h.state.value = SandboxState.RUNNING;
    });
    const events = await h.clock.run(
      collect(h.client.sandbox(SANDBOX).exec({ command: ["x"] })),
    );
    expect(stdoutText(events)).toBe("ab");
    expect(h.clock.now()).toBeGreaterThan(150_000);
  });

  test("ends with the sandbox end reason when a paused sandbox ends", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.paused());
    });
    h.state.value = SandboxState.DELETED;
    h.api.unary(
      (await import("@delino/ownsand-protos/v1/sandbox_pb")).SandboxService,
      "GetSandbox",
      () => ({
        sandbox: {
          sandboxId: SANDBOX,
          state: SandboxState.DELETED,
          endReason: SandboxEndReason.IDLE_TTL_EXPIRED,
        },
      }),
    );
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result).toMatchObject({
      kind: ExecResultKind.SandboxEnded,
      reason: SandboxEndReason.IDLE_TTL_EXPIRED,
    });
  });

  test("retries a stream rejected at agent capacity, also twice, within the window", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* (_req, call) {
      if (call.index < 2) {
        throw ownsandError(Code.ResourceExhausted, "agent stream capacity");
      }
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      if (call.index < 1) {
        throw ownsandError(Code.ResourceExhausted, "agent stream capacity");
      }
      throw new ConnectError("no exec", Code.NotFound);
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    // A rejection at capacity says nothing about the grant: one
    // authorization, and the same grant ID in the header of every call.
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
    const calls = [...h.api.callsOf("Exec"), ...h.api.callsOf("AttachExec")];
    expect(calls.length).toBeGreaterThanOrEqual(4);
    for (const call of calls) {
      expect(call.headers.get("ownsand-relay-grant-id")).toBe("grant-1");
    }
  });

  test("renews the grant with replaces_grant_id after an unavailable stream", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* (_req, call) {
      if (call.index === 0) {
        throw ownsandError(Code.Unavailable, "agent unreachable");
      }
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      throw new ConnectError("no exec", Code.NotFound);
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(auths).toHaveLength(2);
    expect(auths[1].replacesGrantId).toBe("grant-1");
  });

  test("waits at least the retry delay after AuthorizeRelay fails with resource_exhausted", async () => {
    const h = setup();
    const times: number[] = [];
    h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => {
      times.push(h.clock.now());
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "grant limit", {
          retryAfterMs: 20_000,
        });
      }
      return { grantId: "g", expiresIn: { seconds: 900n } };
    });
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    const auths = h.api.callsOf("AuthorizeRelay").map((c) => c.request as any);
    expect(times[1]! - times[0]!).toBeGreaterThanOrEqual(20_000);
    expect(auths[1].authorizationId).toBe(auths[0].authorizationId);
  });

  test("retries a lost AuthorizeRelay response with the same authorization_id", async () => {
    const h = setup();
    const issued: string[] = [];
    h.api.unary(DataPathService, "AuthorizeRelay", (req, call) => {
      issued.push(req.authorizationId);
      if (call.index === 0) {
        // The control plane created the grant but the response was lost.
        throw new LostResponse();
      }
      return { grantId: "grant-a", expiresIn: { seconds: 900n } };
    });
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(issued).toHaveLength(2);
    expect(issued[0]).toBe(issued[1]);
  });

  describe("a stalled AuthorizeRelay call", () => {
    /** A response that never arrives; the fake aborts it with its signal. */
    const stall = () => new Promise<never>(() => {});

    test("ends the operation with a DataPathError at the end of its own 60-second window, also without a signal", async () => {
      const h = setup();
      h.api.unary(DataPathService, "AuthorizeRelay", stall);
      const outcome = h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait()
        .catch((e) => e);
      let settled = false;
      outcome.then(() => (settled = true));
      await h.clock.advance(60_000 - 1);
      expect(settled).toBe(false);
      await h.clock.advance(1);
      const error = await outcome;
      expect(error).toBeInstanceOf(DataPathError);
      expect(error.code).toBe(Code.Unavailable);
      expect(error.outcomeUnknown).toBe(false);
      expect(h.clock.now()).toBe(60_000);
      // The deadline is not retried and sends no Connect timeout.
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
      expect(h.api.calls[0]!.headers.get("connect-timeout-ms")).toBeNull();
      expect(h.api.callsOf("Exec")).toHaveLength(0);
    });

    test("ends a recovering operation with a DataPathError when the recovery window ends", async () => {
      const h = setup();
      h.api.stream(RelayService, "Exec", async function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.started());
        yield new Sleep(10_000);
        yield new Disconnect();
      });
      h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) =>
        call.index === 0
          ? { grantId: "g", expiresIn: { seconds: 900n } }
          : stall(),
      );
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(DataPathError);
      expect(error.code).toBe(Code.Unavailable);
      // The window starts at the interruption (10 s) and lasts 60 s.
      expect(h.clock.now()).toBe(70_000);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
    });

    test("ends with the caller's abort error when the caller cancels first", async () => {
      const h = setup();
      h.api.unary(DataPathService, "AuthorizeRelay", stall);
      const abort = new AbortController();
      const reason = new Error("caller gave up");
      const outcome = h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"], signal: abort.signal })
        .wait()
        .catch((e) => e);
      await h.clock.advance(10_000);
      abort.abort(reason);
      const error = await outcome;
      expect(error).toBe(reason);
      expect(h.clock.now()).toBe(10_000);
    });

    test("uses a grant that arrives within the window", async () => {
      const h = setup();
      h.api.unary(DataPathService, "AuthorizeRelay", async () => {
        await h.clock.sleep(5_000);
        return { grantId: "late", expiresIn: { seconds: 900n } };
      });
      h.api.stream(RelayService, "Exec", async function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.exited(0));
      });
      const result = await h.clock.run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      );
      expect(result.kind).toBe(ExecResultKind.Exited);
      expect(h.clock.now()).toBe(5_000);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
      expect(
        h.api.callsOf("Exec")[0]!.headers.get("ownsand-relay-grant-id"),
      ).toBe("late");
    });

    test("ends with the last error when an attempt fails as the window ends", async () => {
      const h = setup();
      h.api.unary(DataPathService, "AuthorizeRelay", async () => {
        // The response arrives with an error just before the window ends.
        await h.clock.sleep(59_900);
        throw ownsandError(Code.Unavailable, "sandbox unreachable");
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.Unavailable);
      expect(h.clock.now()).toBe(59_900);
    });
  });

  test("retries a lost renewal response with the same authorization_id and replaces_grant_id", async () => {
    const h = setup();
    const requests: any[] = [];
    h.api.unary(DataPathService, "AuthorizeRelay", (req, call) => {
      requests.push(req);
      if (call.index === 1) {
        throw new LostResponse();
      }
      return { grantId: `grant-${call.index}`, expiresIn: { seconds: 900n } };
    });
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      for (let i = 0; i < 13; i++) {
        yield new Sleep(MIN);
        yield execMsg(ev.keepalive());
      }
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(requests).toHaveLength(3);
    expect(requests[2].authorizationId).toBe(requests[1].authorizationId);
    expect(requests[2].replacesGrantId).toBe("grant-0");
    expect(requests[1].replacesGrantId).toBe("grant-0");
  });

  test.each([
    ["AGENT_CHANNEL_LOST", SessionEndReason.AGENT_CHANNEL_LOST],
    ["GRANT_INVALID", SessionEndReason.GRANT_INVALID],
    ["GRANT_REPLACED", SessionEndReason.GRANT_REPLACED],
  ])("recovers with back-off after %s", async (_name, reason) => {
    const h = setup();
    // Record the virtual time of every AuthorizeRelay attempt. This replaces
    // the harness handler and keeps its behavior: one grant per authorization.
    const attemptTimes: number[] = [];
    const grantByAuthorization = new Map<string, string>();
    h.api.unary(DataPathService, "AuthorizeRelay", (req) => {
      attemptTimes.push(h.clock.now());
      let grant = grantByAuthorization.get(req.authorizationId);
      if (grant === undefined) {
        grant = `grant-${grantByAuthorization.size + 1}`;
        grantByAuthorization.set(req.authorizationId, grant);
      }
      return {
        grantId: grant,
        expiresIn: { seconds: 900n },
        expireTime: { seconds: 0n },
      };
    });
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      throw ownsandError(Code.Unavailable, "session ended", {
        sessionEndReason: reason,
      });
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      if (call.index < 3) {
        throw ownsandError(Code.Unavailable, "no relay connection");
      }
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(5);
    // The recovery window starts at 500 ms and doubles up to 5 s.
    expect(attemptTimes).toHaveLength(5);
    const gaps = attemptTimes.slice(1).map((at, i) => at - attemptTimes[i]);
    expect(gaps).toEqual([500, 1000, 2000, 4000]);
  });

  test("keeps retrying while the sandbox is UNREACHABLE during a control plane deployment, then gives up after 60 seconds", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    let reachableAt = 40_000;
    h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => {
      if (call.index > 0 && h.clock.now() < reachableAt) {
        throw ownsandError(Code.Unavailable, "sandbox unreachable");
      }
      return { grantId: `g${call.index}`, expiresIn: { seconds: 900n } };
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.clock.now()).toBeGreaterThanOrEqual(40_000);

    const h2 = setup();
    h2.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    reachableAt = Number.POSITIVE_INFINITY;
    h2.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => {
      if (call.index > 0) {
        throw ownsandError(Code.Unavailable, "sandbox unreachable");
      }
      return { grantId: "g", expiresIn: { seconds: 900n } };
    });
    const error = await h2.clock
      .run(
        h2.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect(error.code).toBe(Code.Unavailable);
    expect(h2.clock.now()).toBeLessThanOrEqual(61_000);
  });

  test("ends at once when the credential was revoked", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => {
      if (call.index > 0) {
        throw new ConnectError("api key revoked", Code.Unauthenticated);
      }
      return { grantId: "g", expiresIn: { seconds: 900n } };
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error.code).toBe(Code.Unauthenticated);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
  });

  test("an explicit aborted status with an ErrorDetail is terminal; without one it is an interruption", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* (_req, call) {
      if (call.index === 0) {
        throw new ConnectError("connection closed", Code.Aborted);
      }
      throw ownsandError(Code.Aborted, "explicit");
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      throw new ConnectError("no exec", Code.NotFound);
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error.code).toBe(Code.Aborted);
    expect(h.api.callsOf("Exec")).toHaveLength(2);
  });

  test("a stream that ends without its end-of-stream message is an interruption", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Eof();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
  });

  test("attaches to an exec it did not start with an ATTACH_EXEC grant without a digest", async () => {
    const h = setup();
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.started());
      yield attachMsg(ev.stdout(5, "tail"));
      yield attachMsg(ev.exited(0));
    });
    const execId = "0192f4b6-8000-7000-8000-000000000001";
    const events = await h.clock.run(
      collect(
        h.client.sandbox(SANDBOX).attachExec(execId, { stdoutOffset: 5n }),
      ),
    );
    expect(stdoutText(events)).toBe("tail");
    const auth = h.api.callsOf("AuthorizeRelay")[0]!.request as any;
    expect(auth.operationKind).toBe(RelayOperationKind.ATTACH_EXEC);
    expect(auth.operationId).toBe(execId);
    expect(auth.requestDigest).toHaveLength(0);
  });

  test.each([
    ["an empty exec ID", "", {}, "exec_id", "is required"],
    [
      "a malformed exec ID",
      "not-a-uuid",
      {},
      "exec_id",
      "must be a canonical UUID v7",
    ],
    [
      "a UUID v4",
      "0192f4b6-8000-4000-8000-000000000001",
      {},
      "exec_id",
      "must be a canonical UUID v7",
    ],
    [
      "an uppercase exec ID",
      "0192F4B6-8000-7000-8000-00000000000A",
      {},
      "exec_id",
      "must be a canonical lowercase UUID v7",
    ],
    [
      "a negative stdout offset",
      "0192f4b6-8000-7000-8000-000000000001",
      { stdoutOffset: -1n },
      "stdout_offset",
      "must not be negative",
    ],
    [
      "a negative stderr offset",
      "0192f4b6-8000-7000-8000-000000000001",
      { stderrOffset: -1n },
      "stderr_offset",
      "must not be negative",
    ],
    [
      "two negative offsets (stdout is reported first)",
      "0192f4b6-8000-7000-8000-000000000001",
      { stdoutOffset: -1n, stderrOffset: -1n },
      "stdout_offset",
      "must not be negative",
    ],
    [
      "a bad exec ID and a negative offset (the exec ID is reported first)",
      "x",
      { stdoutOffset: -1n },
      "exec_id",
      "must be a canonical UUID v7",
    ],
  ])(
    "rejects %s in attachExec synchronously before any request",
    (_name, execId, options, field, rule) => {
      const h = setup();
      let error: unknown;
      try {
        h.client.sandbox(SANDBOX).attachExec(execId, options);
      } catch (e) {
        error = e;
      }
      expect(error).toBeInstanceOf(OwnSandValidationError);
      expect((error as OwnSandValidationError).field).toBe(field);
      expect((error as OwnSandValidationError).message).toContain(rule);
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test("attaches with offset zero explicitly and with large offsets", async () => {
    const h = setup();
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    await h.clock.run(
      collect(
        h.client
          .sandbox(SANDBOX)
          .attachExec("0192f4b6-8000-7000-8000-000000000001", {
            stdoutOffset: 0n,
            stderrOffset: 2n ** 40n,
          }),
      ),
    );
    const req = h.api.callsOf("AttachExec")[0]!.request as any;
    expect(req.stdoutOffset).toBe(0n);
    expect(req.stderrOffset).toBe(2n ** 40n);
  });

  test("recovers 11 interrupted operations of one principal at once", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Sleep(1000);
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      if (call.index < 11) {
        throw ownsandError(Code.Unavailable, "control plane restarting");
      }
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const results = await h.clock.run(
      Promise.all(
        Array.from({ length: 11 }, () =>
          sandbox.exec({ command: ["x"] }).wait(),
        ),
      ),
    );
    expect(results.every((r) => r.kind === ExecResultKind.Exited)).toBe(true);
    expect(h.api.callsOf("Exec")).toHaveLength(11);
  });

  test("continues a silent exec late after more than 14 minutes and an interruption", async () => {
    const h = setup();
    function* silentMinutes(minutes: number, msg: typeof execMsg) {
      for (let i = 0; i < minutes; i++) {
        yield new Sleep(MIN);
        yield msg(ev.keepalive());
      }
    }
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield* silentMinutes(13, execMsg);
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      yield attachMsg(ev.accepted());
      if (call.index === 0) {
        yield* silentMinutes(13, attachMsg);
        return;
      }
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.clock.now()).toBeGreaterThan(26 * MIN);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(2);
  });

  test("validates the exec request before sending it", async () => {
    const h = setup();
    const sandbox = h.client.sandbox(SANDBOX);
    expect(() => sandbox.exec({ command: [] })).toThrow("command");
    expect(() =>
      sandbox.exec({ command: ["x", "y".repeat(300 * 1024)] }),
    ).toThrow("command");
    expect(() => sandbox.exec({ command: ["x"], timeoutMs: -1 })).toThrow(
      "timeout",
    );
    expect(() => sandbox.exec({ command: ["x"], timeoutMs: 1e-7 })).toThrow(
      "timeout",
    );
    expect(h.api.calls).toHaveLength(0);
  });

  test("rejects a timeout above the protobuf Duration maximum before sending", async () => {
    const h = setup();
    const sandbox = h.client.sandbox(SANDBOX);
    // 315576000000 seconds is the longest protobuf Duration.
    for (const timeoutMs of [315_576_000_001_000, 3.2e14, 1e300]) {
      expect(() => sandbox.exec({ command: ["x"], timeoutMs })).toThrow(
        "is not a valid duration",
      );
    }
    expect(h.api.calls).toHaveLength(0);
  });

  test("accepts a timeout of exactly the protobuf Duration maximum", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(
      sandbox.exec({ command: ["x"], timeoutMs: 315_576_000_000_000 }).wait(),
    );
    const timeout = (h.api.callsOf("Exec")[0]!.request as any).timeout;
    expect(timeout.seconds).toBe(315_576_000_000n);
    expect(timeout.nanos).toBe(0);
  });

  test("sends a fractional millisecond timeout at nanosecond precision", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.started());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(
      sandbox.exec({ command: ["x"], timeoutMs: 1500.5 }).wait(),
    );
    const timeout = (h.api.callsOf("Exec")[0]!.request as any).timeout;
    expect(timeout.seconds).toBe(1n);
    expect(timeout.nanos).toBe(500_000_000 + 500_000);
  });
});

/** Wraps a clock and counts the sleeps that have not settled yet. */
function countingClock(inner: VirtualClock): Clock & { pending: () => number } {
  let pending = 0;
  return {
    now: () => inner.now(),
    wallNow: () => inner.wallNow(),
    pending: () => pending,
    sleep(ms, signal) {
      pending++;
      return inner.sleep(ms, signal).finally(() => {
        pending--;
      });
    },
  };
}

describe("exec handle iteration that ends early", () => {
  /** Yields one stdout chunk and then hangs, like an agent that went silent. */
  function silentAfterFirstChunk(h: ReturnType<typeof setup>) {
    const seen: { signal?: AbortSignal } = {};
    h.api.stream(RelayService, "Exec", async function* (req, call) {
      seen.signal = call.signal;
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.stdout(0, "a"));
      yield new Hang();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.stdout(1, "b"));
      yield attachMsg(ev.exited(0));
    });
    return seen;
  }

  for (const how of ["break", "throw"] as const) {
    test(`aborts the call after the idle limit and leaves no timer or listener after a ${how} in the loop`, async () => {
      let counted: ReturnType<typeof countingClock> | undefined;
      const h = setup((clock) => {
        counted = countingClock(clock);
        return { clock: counted };
      });
      const seen = silentAfterFirstChunk(h);
      const caller = new AbortController();
      const listenersBefore = getEventListeners(caller.signal, "abort").length;
      const handle = h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"], signal: caller.signal });
      await h.clock.run(
        (async () => {
          try {
            for await (const e of handle) {
              if (e.type !== ExecEventType.Stdout) continue;
              if (how === "break") break;
              throw new Error("boom");
            }
          } catch (error) {
            expect((error as Error).message).toBe("boom");
          }
        })(),
      );
      // Not idle yet: the event was released at 0 ms.
      await h.clock.advance(Timing.streamIdleMs - 1_000);
      expect(seen.signal?.aborted).toBe(false);
      await h.clock.advance(2_000);
      expect(seen.signal?.aborted).toBe(true);
      expect(counted!.pending()).toBe(0);
      expect(getEventListeners(caller.signal, "abort")).toHaveLength(
        listenersBefore,
      );
    });
  }

  test("break followed by an immediate wait() resumes the same stream", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* (req) {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.stdout(0, "a"));
      yield execMsg(ev.stdout(1, "b"));
      yield execMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    await h.clock.run(
      (async () => {
        for await (const e of handle) {
          if (e.type === ExecEventType.Stdout) break;
        }
      })(),
    );
    const result = await h.clock.run(handle.wait());
    expect(result).toMatchObject({ kind: ExecResultKind.Exited, exitCode: 0n });
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
  });

  test("wait() after the idle limit released the stream continues with AttachExec from the delivered offsets", async () => {
    const h = setup();
    const seen = silentAfterFirstChunk(h);
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    await h.clock.run(
      (async () => {
        for await (const e of handle) {
          if (e.type === ExecEventType.Stdout) break;
        }
      })(),
    );
    await h.clock.advance(Timing.streamIdleMs + 1_000);
    expect(seen.signal?.aborted).toBe(true);
    const result = await h.clock.run(handle.wait());
    expect(result).toMatchObject({
      kind: ExecResultKind.Exited,
      exitCode: 0n,
      complete: true,
    });
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
    const attach = h.api.callsOf("AttachExec")[0]!.request as any;
    expect(attach.stdoutOffset).toBe(1n);
    expect(attach.stderrOffset).toBe(0n);
  });

  test("the caller aborting its own signal after a break ends the watcher and leaves no timer", async () => {
    let counted: ReturnType<typeof countingClock> | undefined;
    const h = setup((clock) => {
      counted = countingClock(clock);
      return { clock: counted };
    });
    const seen = silentAfterFirstChunk(h);
    const caller = new AbortController();
    const listenersBefore = getEventListeners(caller.signal, "abort").length;
    const handle = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"], signal: caller.signal });
    await h.clock.run(
      (async () => {
        for await (const e of handle) {
          if (e.type === ExecEventType.Stdout) break;
        }
      })(),
    );
    expect(counted!.pending()).toBeGreaterThan(0);
    caller.abort(new Error("caller stop"));
    await h.clock.settle();
    expect(seen.signal?.aborted).toBe(true);
    expect(counted!.pending()).toBe(0);
    expect(getEventListeners(caller.signal, "abort")).toHaveLength(
      listenersBefore,
    );
  });

  test("a consumer that still holds an event keeps the idle clock paused", async () => {
    const h = setup();
    const seen = silentAfterFirstChunk(h);
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    const iterator = handle[Symbol.asyncIterator]();
    await h.clock.run(
      (async () => {
        for (;;) {
          const next = await iterator.next();
          if (next.value?.type === ExecEventType.Stdout) return;
        }
      })(),
    );
    await h.clock.advance(10 * Timing.streamIdleMs);
    expect(seen.signal?.aborted).toBe(false);
    await iterator.return(undefined);
    await h.clock.advance(Timing.streamIdleMs + 1_000);
    expect(seen.signal?.aborted).toBe(true);
  });
});

describe("a pending accessToken callback", () => {
  /** A token source whose promise the test settles by hand. */
  function deferredToken() {
    let resolve!: (token: string) => void;
    const promise = new Promise<string>((r) => (resolve = r));
    let calls = 0;
    return {
      accessToken: () => {
        calls++;
        return promise;
      },
      resolve,
      calls: () => calls,
    };
  }

  function relayHarness(accessToken: () => string | Promise<string>) {
    // setup() spreads these options after apiKey, so clear the API key.
    const h = setup({ apiKey: undefined, accessToken });
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    return h;
  }

  test("ends the operation with the caller's abort reason without waiting for the callback", async () => {
    const token = deferredToken();
    const h = relayHarness(token.accessToken);
    const caller = new AbortController();
    const reason = new Error("caller gave up");
    const outcome = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"], signal: caller.signal })
      .wait()
      .catch((e) => e);
    await h.clock.advance(10_000);
    expect(token.calls()).toBe(1);
    caller.abort(reason);
    // The callback is still pending: the abort alone ends the operation.
    expect(await outcome).toBe(reason);
    expect(h.clock.now()).toBe(10_000);
    expect(h.api.calls).toHaveLength(0);
  });

  test("ends the operation with the existing deadline error and sends no request when the 60-second window ends", async () => {
    const token = deferredToken();
    const h = relayHarness(token.accessToken);
    const outcome = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"] })
      .wait()
      .catch((e) => e);
    let settled = false;
    outcome.then(() => (settled = true));
    await h.clock.advance(60_000 - 1);
    expect(settled).toBe(false);
    await h.clock.advance(1);
    const error = await outcome;
    expect(error).toBeInstanceOf(DataPathError);
    expect(error.code).toBe(Code.Unavailable);
    expect(h.clock.now()).toBe(60_000);
    expect(h.api.calls).toHaveLength(0);
    // A token that arrives after the deadline starts no request.
    token.resolve("late-token");
    await h.clock.settle();
    expect(h.api.calls).toHaveLength(0);
  });

  test("starts no request when the callback resolves after the caller aborted", async () => {
    const token = deferredToken();
    const h = relayHarness(token.accessToken);
    const caller = new AbortController();
    const reason = new Error("caller gave up");
    const outcome = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"], signal: caller.signal })
      .wait()
      .catch((e) => e);
    await h.clock.advance(1_000);
    caller.abort(reason);
    expect(await outcome).toBe(reason);
    token.resolve("late-token");
    await h.clock.settle();
    expect(h.api.calls).toHaveLength(0);
  });

  test("uses a token that resolves within the window as the bearer secret", async () => {
    const token = deferredToken();
    const h = relayHarness(token.accessToken);
    const outcome = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"] })
      .wait();
    await h.clock.advance(5_000);
    expect(h.api.calls).toHaveLength(0);
    token.resolve("good-token");
    const result = await h.clock.run(outcome);
    expect(result.kind).toBe(ExecResultKind.Exited);
    const [auth] = h.api.callsOf("AuthorizeRelay");
    const [exec] = h.api.callsOf("Exec");
    expect(auth!.headers.get("authorization")).toBe("Bearer good-token");
    expect(exec!.headers.get("authorization")).toBe("Bearer good-token");
    expect(token.calls()).toBe(2);
  });
});

/** Runs fn after ms of virtual time. */
function setTimeoutVirtual(
  h: { clock: { sleep(ms: number): Promise<void> } },
  ms: number,
  fn: () => void,
): void {
  void h.clock.sleep(ms).then(fn);
}
