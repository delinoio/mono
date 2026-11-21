// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { execBinding, requestDigest } from "./digest.ts";
import { DataPathError } from "./errors.ts";
import {
  DataPathTransport,
  RelayOperationKind,
  SandboxEndReason,
  SandboxState,
  SessionEndReason,
} from "./gen/delino/ownsand/v1/common_pb.ts";
import { DataPathService } from "./gen/delino/ownsand/v1/data_path_pb.ts";
import { RelayService } from "./gen/delino/ownsand/v1/relay_pb.ts";
import {
  ExecEventType,
  ExecResultKind,
  type ExecStreamEvent,
} from "./operations/exec.ts";
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

describe("exec over the relay", () => {
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
    const handle = sandbox.exec({ command: ["/bin/echo", "hello world"] });
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
      // A server clock 10 hours ahead of the caller.
      expireTime: { seconds: BigInt(Math.floor(Date.now() / 1000) + 36_000) },
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
    expect(error).toBeDefined();
    expect(h.clock.now() - started).toBeLessThanOrEqual(61_000);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec").length).toBeGreaterThan(2);
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
    h.api.stream(RelayService, "AttachExec", async function* () {
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
    expect(error).toBeDefined();
    // One continuation of about 90 seconds already exceeds the window.
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
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
    expect(error).toBeDefined();
    expect(h.clock.now()).toBeLessThanOrEqual(61_000);
    expect(h.api.callsOf("AttachExec").length).toBeGreaterThanOrEqual(2);
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
      (await import("./gen/delino/ownsand/v1/sandbox_pb.ts")).SandboxService,
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
    const times = h.api.callsOf("AuthorizeRelay").length;
    expect(times).toBe(5);
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
    expect(h.api.calls).toHaveLength(0);
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
