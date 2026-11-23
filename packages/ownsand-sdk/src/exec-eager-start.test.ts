// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import {
  ExecEventType,
  ExecResultKind,
  type ExecStreamEvent,
} from "./operations/exec.ts";
import { Timing } from "./relay/context.ts";
import { Hang } from "./testing/fake-api.ts";
import {
  attachMsg,
  ev,
  execMsg,
  SANDBOX,
  setup,
  type Harness,
} from "./testing/harness.ts";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { uuidV7TimestampMs } from "./uuidv7.ts";

const MIN = 60_000;

/**
 * An agent that applies the operation ID admission window of
 * docs/packages-ownsand-sdk-foundation.md to the first ExecRequest: an
 * unknown exec_id that is 15 minutes old or older is `not_found`.
 */
function admittingAgent(h: Harness) {
  const seen: {
    signal?: AbortSignal;
    execIds: string[];
    attachExecIds: string[];
  } = { execIds: [], attachExecIds: [] };
  h.api.stream(RelayService, "Exec", async function* (req, call) {
    seen.signal = call.signal;
    seen.execIds.push(req.execId);
    if (
      h.clock.wallNow() - uuidV7TimestampMs(req.execId) >=
      Timing.operationIdLifetimeMs
    ) {
      throw new ConnectError("unknown operation id", Code.NotFound);
    }
    yield execMsg(ev.accepted());
    yield execMsg(ev.started(req.execId));
    yield execMsg(ev.stdout(0, "a"));
    yield execMsg(ev.exited(0));
  });
  // The agent keeps the exec record, so a continuation by exec_id is not
  // subject to the admission window of an unknown ID. The idle limit ends an
  // unread stream, so a read after a long delay continues through here.
  h.api.stream(RelayService, "AttachExec", async function* (req) {
    seen.attachExecIds.push(req.execId);
    yield attachMsg(ev.accepted());
    yield attachMsg(ev.stdout(0, "a"));
    yield attachMsg(ev.exited(0));
  });
  return seen;
}

describe("an exec starts when exec() returns", () => {
  test("sends the first request before the handle is read", async () => {
    const h = setup();
    const seen = admittingAgent(h);
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    await h.clock.settle();
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(seen.execIds).toEqual([handle.execId]);
    expect(await h.clock.run(handle.wait())).toMatchObject({
      kind: ExecResultKind.Exited,
      exitCode: 0n,
    });
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
  });

  test("wait() after the admission window still returns the result", async () => {
    const h = setup();
    const seen = admittingAgent(h);
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    await h.clock.advance(Timing.operationIdLifetimeMs + MIN);
    const result = await h.clock.run(handle.wait());
    expect(result).toMatchObject({ kind: ExecResultKind.Exited, exitCode: 0n });
    expect(seen.execIds).toEqual([handle.execId]);
    expect(seen.attachExecIds).toEqual([handle.execId]);
  });

  test("the first iteration after the admission window sees every event once", async () => {
    const h = setup();
    const seen = admittingAgent(h);
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    const execId = handle.execId;
    await h.clock.advance(Timing.operationIdLifetimeMs + MIN);
    const events: ExecStreamEvent[] = [];
    await h.clock.run(
      (async () => {
        for await (const e of handle) events.push(e);
      })(),
    );
    expect(events.map((e) => e.type)).toEqual([
      ExecEventType.Started,
      ExecEventType.Stdout,
    ]);
    expect(handle.execId).toBe(execId);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(seen.attachExecIds).toEqual([execId]);
  });

  test("a failed first send is reported by the first read and leaves no unhandled rejection", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      throw new ConnectError("bad request", Code.InvalidArgument);
      yield execMsg(ev.accepted());
    });
    const unhandled: unknown[] = [];
    const onUnhandled = (reason: unknown) => unhandled.push(reason);
    process.on("unhandledRejection", onUnhandled);
    try {
      const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
      // Give the unread handle's first read time to fail.
      await h.clock.settle();
      expect(unhandled).toEqual([]);
      const first = await h.clock.run(handle.wait().catch((e: unknown) => e));
      expect(ConnectError.from(first).code).toBe(Code.InvalidArgument);
      const second = await handle.wait().catch((e: unknown) => e);
      expect(second).toBe(first);
      await h.clock.settle();
      expect(unhandled).toEqual([]);
    } finally {
      process.off("unhandledRejection", onUnhandled);
    }
  });

  test("a caller abort before the first read is reported by the first read", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield new Hang();
    });
    const caller = new AbortController();
    const reason = new Error("caller gave up");
    const handle = h.client
      .sandbox(SANDBOX)
      .exec({ command: ["x"], signal: caller.signal });
    await h.clock.settle();
    caller.abort(reason);
    await h.clock.settle();
    expect(await h.clock.run(handle.wait().catch((e: unknown) => e))).toBe(
      reason,
    );
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("an unread handle ends its stream at the idle limit and a later wait() attaches", async () => {
    const h = setup();
    const seen: { signal?: AbortSignal } = {};
    h.api.stream(RelayService, "Exec", async function* (req, call) {
      seen.signal = call.signal;
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield new Hang();
    });
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    await h.clock.advance(Timing.streamIdleMs - 1_000);
    expect(seen.signal?.aborted).toBe(false);
    await h.clock.advance(2_000);
    expect(seen.signal?.aborted).toBe(true);
    const result = await h.clock.run(handle.wait());
    expect(result).toMatchObject({ kind: ExecResultKind.Exited, exitCode: 0n });
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
  });

  test("a reader that holds the first event keeps the idle clock paused", async () => {
    const h = setup();
    const seen: { signal?: AbortSignal } = {};
    h.api.stream(RelayService, "Exec", async function* (req, call) {
      seen.signal = call.signal;
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield new Hang();
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["x"] });
    const iterator = handle[Symbol.asyncIterator]();
    const first = await h.clock.run(iterator.next());
    expect(first.value?.type).toBe(ExecEventType.Started);
    await h.clock.advance(10 * Timing.streamIdleMs);
    expect(seen.signal?.aborted).toBe(false);
    await iterator.return(undefined);
    await h.clock.advance(Timing.streamIdleMs + 1_000);
    expect(seen.signal?.aborted).toBe(true);
  });

  test("attachExec still sends nothing before the handle is read", async () => {
    const h = setup();
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const handle = h.client
      .sandbox(SANDBOX)
      .attachExec("0192f4b6-7c3a-7def-8abc-0123456789ac");
    await h.clock.settle();
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
    await h.clock.run(handle.wait());
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
  });
});
