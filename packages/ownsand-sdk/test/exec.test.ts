// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";

import { DataPathError } from "../src/errors.ts";
import {
  RelayOperationKind,
  SandboxEndReason,
  SandboxState,
  SessionEndReason,
} from "../src/gen/delino/ownsand/v1/common_pb.ts";
import {
  ExecEventKind,
  type ExecResult,
  ExecResultKind,
  type ExecStreamEvent,
} from "../src/relay/exec.ts";
import type { ExecRun } from "../src/sandbox.ts";
import { disconnect, exhausted, sessionEnd } from "./support/fake.ts";
import { type Harness, harness, rejection, text } from "./support/harness.ts";

const MIN = 60_000;

async function drain(
  run: ExecRun,
): Promise<{ events: ExecStreamEvent[]; result: ExecResult }> {
  const events: ExecStreamEvent[] = [];
  for await (const ev of run) {
    events.push(ev);
  }
  return { events, result: await run.wait() };
}

function stdoutText(events: ExecStreamEvent[]): string {
  return events
    .filter((e) => e.kind === ExecEventKind.STDOUT)
    .map((e) => text((e as { data: Uint8Array }).data))
    .join("");
}

function expectClean(h: Harness): void {
  expect(h.fake.violations).toEqual([]);
  expect(h.fake.execsRunTwice()).toEqual([]);
}

describe("exec over the relay", () => {
  test("reports output, the exit code, one started event and no relay-internal events", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 1_000, stderr: "e" },
      { delayMs: 1_000, exit: 7 },
    ]);
    const { events, result } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(events.map((e) => e.kind)).toEqual([
      ExecEventKind.STARTED,
      ExecEventKind.STDOUT,
      ExecEventKind.STDERR,
    ]);
    expect(result).toMatchObject({
      kind: ExecResultKind.EXITED,
      exitCode: 7n,
      timedOut: false,
      complete: true,
    });
    expectClean(h);
  });

  test("reports a command that could not start, a signal and a sandbox end as distinct results", async () => {
    const h = harness();
    h.fake.programs.set("missing", [{ delayMs: 0, failed: "no such file" }]);
    h.fake.programs.set("killed", [{ delayMs: 0, signal: 9 }]);
    h.fake.programs.set("long", [{ delayMs: 10 * MIN, exit: 0 }]);
    expect((await h.run(h.sandbox.exec(["missing"]).wait())).kind).toBe(
      ExecResultKind.FAILED,
    );
    expect(await h.run(h.sandbox.exec(["killed"]).wait())).toMatchObject({
      kind: ExecResultKind.EXITED,
      signal: 9,
    });
    const run = h.sandbox.exec(["long"]).wait();
    void h.clock
      .sleep(MIN)
      .then(() =>
        h.fake.setSandboxState(
          SandboxState.DELETED,
          SandboxEndReason.MAX_TTL_EXPIRED,
        ),
      );
    expect(await h.run(run)).toMatchObject({
      kind: ExecResultKind.SANDBOX_ENDED,
      reason: SandboxEndReason.MAX_TTL_EXPIRED,
    });
  });

  test("rejects an oversized request before sending it", async () => {
    const h = harness();
    expect(() => h.sandbox.exec(["x".repeat(300 * 1024)])).toThrow(/256 KiB/);
    expect(() => h.sandbox.exec([])).toThrow(/program/);
    expect(() => h.sandbox.exec(["x"], { timeoutMs: 0 })).toThrow(/timeoutMs/);
    expect(h.fake.calls).toHaveLength(0);
  });

  test("continues with AttachExec from the delivered offsets after a connection loss and never runs the exec again", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "aa" },
      { delayMs: 1_000, stderr: "e" },
      { delayMs: 1_000, stdout: "bb" },
      { delayMs: 1_000, exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectAfterData: 2 } : undefined;
    const { events, result } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(stdoutText(events)).toBe("aabb");
    expect(events.filter((e) => e.kind === ExecEventKind.STARTED)).toHaveLength(
      1,
    );
    expect(result.complete).toBe(true);
    const attach = h.fake.callsOf("AttachExec");
    expect(attach).toHaveLength(1);
    expect([attach[0]!.stdoutOffset, attach[0]!.stderrOffset]).toEqual([
      2n,
      1n,
    ]);
    // The continuation was authorized again with the kept request fields.
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    expect(auth[1]!.kind).toBe(RelayOperationKind.EXEC);
    expect(
      Buffer.from(auth[1]!.digest!).equals(Buffer.from(auth[0]!.digest!)),
    ).toBe(true);
    expect(auth[1]!.authorizationId).not.toBe(auth[0]!.authorizationId);
    expect(auth[1]!.replaces).toBe(h.fake.callsOf("Exec")[0]!.grantId);
    expectClean(h);
  });

  test("before started, tries AttachExec from zero and sends the identical ExecRequest only on not_found", async () => {
    const h = harness();
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectBeforeAccept: true } : undefined;
    const result = await h.run(
      h.sandbox.exec(["true"], { env: { B: "2", A: "1" } }).wait(),
    );
    expect(result.kind).toBe(ExecResultKind.EXITED);
    expect(
      h.fake.calls
        .filter((c) => c.rpc === "Exec" || c.rpc === "AttachExec")
        .map((c) => c.rpc),
    ).toEqual(["Exec", "AttachExec", "Exec"]);
    const attach = h.fake.callsOf("AttachExec")[0]!;
    expect([attach.stdoutOffset, attach.stderrOffset]).toEqual([0n, 0n]);
    const execs = h.fake.callsOf("Exec");
    expect(
      Buffer.from(execs[1]!.salt!).equals(Buffer.from(execs[0]!.salt!)),
    ).toBe(true);
    expectClean(h);
  });

  test("a stream rejected at agent capacity is retried", async () => {
    const h = harness();
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { reject: exhausted() } : undefined;
    expect((await h.run(h.sandbox.exec(["true"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );
    expectClean(h);
  });

  test("a continuation rejected twice at agent capacity succeeds within the window", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 5_000, stdout: "b", exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : n === 1 || n === 2
          ? { reject: exhausted() }
          : undefined;
    const { events } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(stdoutText(events)).toBe("ab");
    expect(h.fake.callsOf("AttachExec")).toHaveLength(3);
    expectClean(h);
  });

  test("a stream that ends without accepted is a failure, not a rotation", async () => {
    const h = harness();
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { endBeforeAccept: true } : undefined;
    expect((await h.run(h.sandbox.exec(["true"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    expect(auth[1]!.replaces).toBe(h.fake.callsOf("Exec")[0]!.grantId);
    expect(h.fake.callsOf("AttachExec")).toHaveLength(1);

    // Repeated ends without accepted exhaust the window like other failures.
    const h2 = harness();
    h2.fake.programs.set("p", [{ delayMs: 10 * MIN, exit: 0 }]);
    h2.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterMs: 1_000 }
        : { endBeforeAccept: true, acceptDelayMs: 10_000 };
    const err = await h2.run(rejection(h2.sandbox.exec(["p"]).wait()));
    expect(err).toBeInstanceOf(DataPathError);
    expect((err as ConnectError).code).toBe(Code.Unavailable);
  });

  test("an accepted stream that receives no frame for 90 seconds is cancelled and continued", async () => {
    const h = harness();
    h.fake.programs.set("p", [{ delayMs: 5 * MIN, exit: 0 }]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { stallAfterAccept: true } : undefined;
    expect((await h.run(h.sandbox.exec(["p"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );
    const [first, second] = [
      h.fake.callsOf("Exec")[0]!,
      h.fake.callsOf("AttachExec")[0]!,
    ];
    expect(second.at - first.at).toBeGreaterThanOrEqual(90_000);
    expect(second.at - first.at).toBeLessThan(100_000);
    expectClean(h);
  });

  test("a silent command longer than 14 minutes rotates without limit and renews by replacement on a one-grant device", async () => {
    const h = harness();
    h.fake.grantLimit = 1;
    h.fake.programs.set("silent", [
      { delayMs: 100 * MIN, stdout: "done", exit: 0 },
    ]);
    const { events, result } = await h.run(drain(h.sandbox.exec(["silent"])));
    expect(stdoutText(events)).toBe("done");
    expect(result.kind).toBe(ExecResultKind.EXITED);
    const attach = h.fake.callsOf("AttachExec");
    expect(attach.length).toBeGreaterThanOrEqual(7);
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(attach.length + 1);
    // Every renewal replaces the grant of the previous stream, at the rotation.
    const streams = [...h.fake.callsOf("Exec"), ...attach];
    for (let i = 1; i < auth.length; i++) {
      expect(auth[i]!.replaces).toBe(streams[i - 1]!.grantId);
      expect(auth[i]!.at - auth[0]!.at).toBeGreaterThanOrEqual(
        i * (14 * MIN - 5_000),
      );
    }
    expectClean(h);
  });

  test("a silent exec that recovers once and then rotates cleanly for more than 60 seconds of setup time", async () => {
    const h = harness();
    h.fake.programs.set("silent", [{ delayMs: 70 * MIN, exit: 0 }]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterMs: 2 * MIN }
        : { acceptDelayMs: 20_000, keepaliveBeforeAccept: true };
    expect((await h.run(h.sandbox.exec(["silent"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );
    // Four or more continuations each spent 20 s before accepted: more than 60 s in total.
    expect(h.fake.callsOf("AttachExec").length).toBeGreaterThanOrEqual(4);
    expectClean(h);
  });

  test("a second transport interruption within one recovery window continues the same window", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 3 * MIN, stdout: "b", exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : n === 1
          ? { acceptDelayMs: 20_000, disconnectBeforeAccept: true }
          : undefined;
    const { events } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(stdoutText(events)).toBe("ab");

    const h2 = harness();
    h2.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 10 * MIN, exit: 0 },
    ]);
    h2.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : { acceptDelayMs: 25_000, disconnectBeforeAccept: true };
    const run = h2.sandbox.exec(["p"]);
    const err = await h2.run(rejection(run.wait()));
    expect(err.message).toMatch(/connection reset|aborted|fake/i);
    const attach = h2.fake.callsOf("AttachExec");
    // The window ends about 60 s after the first failure, not after each interruption.
    expect(attach.length).toBeLessThanOrEqual(3);
    expect(h2.clock.now()).toBeLessThan(2 * MIN);
  });

  test("a relay keepalive before accepted leaves the recovery timer running", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 30 * MIN, exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : {
            acceptDelayMs: 40_000,
            keepaliveBeforeAccept: true,
            disconnectBeforeAccept: true,
          };
    const err = await h.run(rejection(h.sandbox.exec(["p"]).wait()));
    expect(err).toBeInstanceOf(Error);
    expect(h.fake.callsOf("AttachExec").length).toBeLessThanOrEqual(2);
    expect(h.clock.now()).toBeLessThan(3 * MIN);
  });

  test("accepted pauses the recovery clock; only data or a terminal result resets it", async () => {
    // Two accepted continuations disconnect at the same offset, each open for
    // 100 s; the window survives because accepted stopped its clock.
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 6 * MIN, stdout: "b", exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : n === 1 || n === 2
          ? { acceptDelayMs: 10_000, disconnectAfterMs: 100_000 }
          : undefined;
    const { events } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(stdoutText(events)).toBe("ab");
    expect(h.fake.callsOf("AttachExec").map((c) => c.stdoutOffset)).toEqual([
      1n,
      1n,
      1n,
    ]);

    // Without data, the time before accepted accumulates and ends the window
    // even though far more than 60 s pass in total.
    const h2 = harness();
    h2.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 60 * MIN, exit: 0 },
    ]);
    h2.fake.hooks.stream = (_rpc, n) =>
      n === 0
        ? { disconnectAfterData: 1 }
        : { acceptDelayMs: 15_000, disconnectAfterMs: 100_000 };
    const err = await h2.run(rejection(h2.sandbox.exec(["p"]).wait()));
    expect(err).toBeInstanceOf(Error);
    expect(h2.clock.now()).toBeGreaterThan(5 * MIN);
    expect(h2.fake.callsOf("AttachExec").length).toBeLessThanOrEqual(4);
  });

  test("a paused sandbox that resumes after 90 seconds before started", async () => {
    const h = harness();
    h.fake.hooks.stream = (_rpc, n) => {
      if (n === 0) {
        h.fake.setSandboxState(SandboxState.PAUSED);
        void h.clock
          .sleep(90_000)
          .then(() => h.fake.setSandboxState(SandboxState.RUNNING));
        return { disconnectBeforeAccept: true };
      }
      return undefined;
    };
    expect((await h.run(h.sandbox.exec(["true"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );
    const execs = h.fake.callsOf("Exec");
    expect(execs).toHaveLength(2);
    expect(execs[1]!.at).toBeGreaterThanOrEqual(90_000);
    expectClean(h);
  });

  test("an existing exec whose AttachExec continuation returns paused resumes after more than 60 seconds", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 10_000, stdout: "b", exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) => {
      if (n === 0) {
        return { disconnectAfterData: 1 };
      }
      if (n === 1) {
        h.fake.setSandboxState(SandboxState.PAUSED);
        void h.clock
          .sleep(2 * MIN)
          .then(() => h.fake.setSandboxState(SandboxState.RUNNING));
      }
      return undefined;
    };
    const { events } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(stdoutText(events)).toBe("ab");
    expect(h.fake.callsOf("AttachExec")).toHaveLength(2);
    expect(h.fake.callsOf("GetSandbox").length).toBeGreaterThan(1);
    expectClean(h);
  });

  test("reports truncated AttachExec output in order and marks the result incomplete", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "aaaa" },
      { delayMs: 1_000, stdout: "bb" },
      { delayMs: 1_000, stdout: "cc" },
      { delayMs: 1_000, exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectAfterData: 1 } : undefined;
    h.fake.hooks.authorize = (req, n) => {
      if (n === 1) {
        // The agent dropped output older than offset 6 before the continuation.
        void h.clock
          .sleep(0)
          .then(() => h.fake.truncateExec(req.operationId, 6n));
      }
      return undefined;
    };
    const { events, result } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(events.map((e) => e.kind)).toEqual([
      ExecEventKind.STARTED,
      ExecEventKind.STDOUT,
      ExecEventKind.TRUNCATED,
      ExecEventKind.STDOUT,
    ]);
    expect(events[2]).toEqual({
      kind: ExecEventKind.TRUNCATED,
      stdout: { from: 4n, to: 6n },
    });
    expect(events[3]).toMatchObject({ offset: 6n });
    expect(result.complete).toBe(false);
  });

  test("attaches to an exec with an ATTACH_EXEC grant and only the exec_id", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "out" },
      { delayMs: 35 * MIN, exit: 4 },
    ]);
    const owner = h.sandbox.exec(["p"]);
    const ownerDone = owner.wait();
    // A late continuation by another program that knows only the exec_id.
    const late = h.clock
      .sleep(16 * MIN)
      .then(() => drain(h.sandbox.attachExec(owner.execId)));
    const [{ events, result }] = await h.run(Promise.all([late, ownerDone]));
    expect(stdoutText(events)).toBe("out");
    expect(result).toMatchObject({ kind: ExecResultKind.EXITED, exitCode: 4n });
    const attachAuth = h.fake
      .callsOf("AuthorizeRelay")
      .filter((c) => c.kind === RelayOperationKind.ATTACH_EXEC);
    // The attach stream rotated once, so the ATTACH_EXEC grant was renewed once.
    expect(attachAuth).toHaveLength(2);
    expect(attachAuth[1]!.replaces).toBeDefined();
    expect(attachAuth[0]!.digest!.byteLength).toBe(0);
    expectClean(h);
  });

  test("an attach-only exec that is unknown ends with a data path error and an unknown outcome", async () => {
    const h = harness();
    const err = await h.run(
      rejection(
        h.sandbox.attachExec("0190f5d6-1c2b-7a3e-8f00-00000000aaaa").wait(),
      ),
    );
    expect(err).toBeInstanceOf(DataPathError);
    expect((err as DataPathError).outcomeUnknown).toBe(true);
    expect((err as DataPathError).code).toBe(Code.NotFound);
  });
});

describe("relay recovery window", () => {
  const failures: [string, () => ConnectError][] = [
    [
      "AGENT_CHANNEL_LOST",
      () => sessionEnd(SessionEndReason.AGENT_CHANNEL_LOST),
    ],
    ["GRANT_INVALID", () => sessionEnd(SessionEndReason.GRANT_INVALID)],
    [
      "a missing agent relay connection",
      () => new ConnectError("no agent relay connection", Code.Unavailable),
    ],
    ["a control plane restart", () => disconnect()],
  ];
  for (const [name, failure] of failures) {
    test(`retries with back-off for 60 seconds after ${name}`, async () => {
      const h = harness();
      h.fake.programs.set("p", [
        { delayMs: 1_000, stdout: "a" },
        { delayMs: 30 * MIN, exit: 0 },
      ]);
      h.fake.hooks.stream = (_rpc, n) =>
        n === 0 ? { disconnectAfterData: 1 } : { reject: failure() };
      const err = await h.run(rejection(h.sandbox.exec(["p"]).wait()));
      expect(err).toBeInstanceOf(Error);
      const attach = h.fake.callsOf("AttachExec");
      const firstFailure = h.fake.callsOf("Exec")[0]!.at + 1_000;
      expect(attach.length).toBeGreaterThanOrEqual(5);
      expect(attach.at(-1)!.at - firstFailure).toBeLessThanOrEqual(60_000);
      expect(attach.at(-1)!.at - firstFailure).toBeGreaterThan(45_000);
      const gaps = attach.slice(1).map((c, i) => c.at - attach[i]!.at);
      expect(gaps[gaps.length - 1]!).toBeGreaterThan(gaps[0]!);
      // Each recovery step authorizes again with the replaced grant.
      expect(
        h.fake
          .callsOf("AuthorizeRelay")
          .slice(1)
          .every((c) => c.replaces !== undefined),
      ).toBe(true);
    });
  }

  test("keeps retrying while the sandbox is UNREACHABLE during a control plane deployment", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 2 * MIN, stdout: "b", exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectAfterData: 1 } : undefined;
    h.fake.hooks.authorize = (_req, n) => {
      if (n === 1) {
        // A control plane deployment: the device is offline until the agent reconnects.
        h.fake.setSandboxState(SandboxState.UNREACHABLE);
        void h.clock
          .sleep(40_000)
          .then(() => h.fake.setSandboxState(SandboxState.RUNNING));
      }
      return undefined;
    };
    const { events } = await h.run(drain(h.sandbox.exec(["p"])));
    expect(stdoutText(events)).toBe("ab");
    expect(h.fake.callsOf("AuthorizeRelay").length).toBeGreaterThan(3);
  });

  test("an operation that starts during a deployment waits for the agent within 60 seconds", async () => {
    const h = harness();
    h.fake.setSandboxState(SandboxState.UNREACHABLE);
    void h.clock
      .sleep(30_000)
      .then(() => h.fake.setSandboxState(SandboxState.RUNNING));
    expect((await h.run(h.sandbox.exec(["true"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );

    const h2 = harness();
    h2.fake.setSandboxState(SandboxState.UNREACHABLE);
    const err = (await h2.run(
      rejection(h2.sandbox.exec(["true"]).wait()),
    )) as ConnectError;
    expect(err.code).toBe(Code.Unavailable);
    expect(h2.clock.now()).toBeLessThanOrEqual(60_000);
    expect(h2.fake.callsOf("Exec")).toHaveLength(0);
  });

  test("stops at once on a terminal sandbox state", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 30 * MIN, exit: 0 },
    ]);
    h.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectAfterData: 1 } : undefined;
    h.fake.hooks.authorize = (_req, n) => {
      if (n === 1) {
        h.fake.setSandboxState(
          SandboxState.FAILED,
          SandboxEndReason.LOST_ON_DEVICE,
        );
      }
      return undefined;
    };
    const err = (await h.run(
      rejection(h.sandbox.exec(["p"]).wait()),
    )) as ConnectError;
    expect(err.code).toBe(Code.FailedPrecondition);
    expect(h.fake.callsOf("AuthorizeRelay")).toHaveLength(2);
  });

  test("a lost AuthorizeRelay response surfaces as aborted and is retried with the same authorization_id", async () => {
    const h = harness();
    h.fake.hooks.authorize = (_req, n) =>
      n === 0 ? { loseResponse: true } : undefined;
    expect((await h.run(h.sandbox.exec(["true"]).wait())).kind).toBe(
      ExecResultKind.EXITED,
    );
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    expect(auth[1]!.authorizationId).toBe(auth[0]!.authorizationId);
    expect(h.fake.grants.size).toBe(1);
  });

  test("waits at least the retry delay of resource_exhausted", async () => {
    const h = harness();
    h.fake.hooks.authorize = (_req, n) =>
      n === 0 ? { reject: exhausted(7_000) } : undefined;
    await h.run(h.sandbox.exec(["true"]).wait());
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth[1]!.at - auth[0]!.at).toBeGreaterThanOrEqual(7_000);
    expect(auth[1]!.authorizationId).toBe(auth[0]!.authorizationId);
  });

  test("recovers 11 interrupted operations of one principal at once", async () => {
    const h = harness();
    h.fake.programs.set("p", [
      { delayMs: 1_000, stdout: "a" },
      { delayMs: 5_000, stdout: "b", exit: 0 },
    ]);
    const firstStreams = new Set<number>();
    h.fake.hooks.stream = (rpc, n) => {
      if (rpc === "Exec" && !firstStreams.has(n) && firstStreams.size < 11) {
        firstStreams.add(n);
        return { disconnectAfterData: 1 };
      }
      return undefined;
    };
    const runs = Array.from({ length: 11 }, () => drain(h.sandbox.exec(["p"])));
    const done = await h.run(Promise.all(runs));
    for (const { events, result } of done) {
      expect(stdoutText(events)).toBe("ab");
      expect(result.kind).toBe(ExecResultKind.EXITED);
    }
    // One grant per operation: the first authorizations name 11 distinct exec_ids.
    const ops = new Set(
      h.fake.callsOf("AuthorizeRelay").map((c) => c.operationId),
    );
    expect(ops.size).toBe(11);
    expectClean(h);
  });

  test("uses one grant per operation and a new salt for each operation", async () => {
    const h = harness();
    await h.run(
      Promise.all([
        h.sandbox.exec(["true"]).wait(),
        h.sandbox.exec(["true"]).wait(),
      ]),
    );
    const auth = h.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    expect(auth[0]!.operationId).not.toBe(auth[1]!.operationId);
    const [a, b] = h.fake.callsOf("Exec");
    expect(a!.grantId).not.toBe(b!.grantId);
    expect(Buffer.from(a!.salt!).equals(Buffer.from(b!.salt!))).toBe(false);
    expect(a!.salt!.byteLength).toBe(32);
  });

  test("the caller can cancel an exec", async () => {
    const h = harness();
    h.fake.programs.set("p", [{ delayMs: 30 * MIN, exit: 0 }]);
    const controller = new AbortController();
    const run = h.sandbox.exec(["p"], { signal: controller.signal });
    void h.clock.sleep(MIN).then(() => controller.abort());
    const err = await h.run(rejection(run.wait()));
    expect(err).toBeInstanceOf(Error);
    expect(h.clock.now()).toBeLessThan(2 * MIN);
  });
});
