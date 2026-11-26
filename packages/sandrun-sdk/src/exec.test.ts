// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { ExecFailureReason } from "@delino/sandrun-protos/v1/common_pb";
import {
  type AttachDevSessionExecRequest,
  AttachDevSessionExecResponseSchema,
  type DevSessionExecEvent,
  type DevSessionExecEventSchema,
  DevSessionService,
  type ExecDevSessionRequest,
  ExecDevSessionResponseSchema,
} from "@delino/sandrun-protos/v1/dev_session_pb";
import { SandRunClient } from "./client.ts";
import { SandRunConfigError } from "./errors.ts";
import {
  type ExecCaller,
  type ExecEvent,
  ExecEventKind,
  execDevSession,
  OutputStream,
  runDevSessionExec,
} from "./exec.ts";
import {
  FAKE_ORIGIN,
  fakeServer,
  RecordingClock,
} from "./testing/fake-server.ts";
import { isUuidV7 } from "./uuidv7.ts";

const ORG = "0193a000-0000-7000-8000-000000000001";
const SESSION = "0193a000-0000-7000-8000-0000000000f1";

type EventInit = MessageInitShape<typeof DevSessionExecEventSchema>["event"];
type Script = (EventInit | ConnectError)[];

const text = new TextEncoder();
const out = (offset: number, data: string): EventInit => ({
  case: "stdout",
  value: { offset: BigInt(offset), data: text.encode(data) },
});
const err = (offset: number, data: string): EventInit => ({
  case: "stderr",
  value: { offset: BigInt(offset), data: text.encode(data) },
});
const started: EventInit = { case: "started", value: { execId: "x" } };
const keepalive: EventInit = { case: "keepalive", value: {} };
const exited = (code: number): EventInit => ({
  case: "exited",
  value: { status: { case: "exitCode", value: BigInt(code) } },
});

const execInput = {
  organizationId: ORG,
  devSessionId: SESSION,
  command: ["true"],
};

function response(event: EventInit) {
  return create(ExecDevSessionResponseSchema, { event: { event } });
}

function execCaller(execDevSession: ExecCaller["execDevSession"]): ExecCaller {
  return {
    execDevSession,
    attachDevSessionExec() {
      throw new Error("unexpected attach");
    },
  };
}

/**
 * A dev session server: ExecDevSession runs `exec`, and the n-th
 * AttachDevSessionExec runs `attaches[n]`. A script entry that is a
 * ConnectError ends the stream with it; a script that runs out ends the
 * stream with an OK status and no final event, as the server does before
 * 14 minutes.
 */
function execServer(exec: Script, attaches: Script[]) {
  const execs: ExecDevSessionRequest[] = [];
  const attachRequests: AttachDevSessionExecRequest[] = [];
  async function* play(script: Script) {
    for (const step of script) {
      if (step instanceof ConnectError) {
        throw step;
      }
      yield { event: { event: step } } as { event: DevSessionExecEvent };
    }
  }
  const server = fakeServer((router) => {
    router.service(DevSessionService, {
      execDevSession(req) {
        execs.push(req);
        return play(exec);
      },
      attachDevSessionExec(req) {
        attachRequests.push(req);
        return play(attaches[attachRequests.length - 1] ?? []);
      },
    });
  });
  return { server, execs, attachRequests };
}

function client(fetch: typeof globalThis.fetch, clock = new RecordingClock()) {
  return new SandRunClient({
    origin: FAKE_ORIGIN,
    fetch,
    token: "sandrun_personal_key",
    clock,
  });
}

function describeEvent(event: ExecEvent): string {
  const decode = (data: Uint8Array) => new TextDecoder().decode(data);
  switch (event.kind) {
    case ExecEventKind.Stdout:
      return `stdout ${event.offset} ${decode(event.data)}`;
    case ExecEventKind.Stderr:
      return `stderr ${event.offset} ${decode(event.data)}`;
    case ExecEventKind.OutputLost:
      return `lost ${event.range.stream} ${event.range.start}-${event.range.end}`;
    case ExecEventKind.Reattached:
      return `reattach ${event.stdoutOffset} ${event.stderrOffset}`;
    case ExecEventKind.Exited:
      return `exited ${event.exitCode}`;
    default:
      return event.kind;
  }
}

describe("exec helper", () => {
  test.each(["throws", "rejects"])(
    "ends the first read when the token source %s",
    async (failure) => {
      const { server, execs, attachRequests } = execServer(
        [started],
        [[new ConnectError("unknown exec", Code.NotFound)]],
      );
      const clock = new RecordingClock();
      const providerError = new Error("fixture token failure");
      let providerCalls = 0;
      const client = new SandRunClient({
        origin: FAKE_ORIGIN,
        fetch: server.fetch,
        clock,
        token: () => {
          if (++providerCalls > 1) {
            return "test-token";
          }
          if (failure === "throws") {
            throw providerError;
          }
          return Promise.reject(providerError);
        },
      });
      const stream = client.execDevSession({
        ...execInput,
        signal: new AbortController().signal,
      });
      const error = (await stream
        .next()
        .catch((error: unknown) => error)) as ConnectError;
      expect(error.cause).toBeInstanceOf(SandRunConfigError);
      expect((error.cause as SandRunConfigError).cause).toBe(providerError);
      expect(providerCalls).toBe(1);
      expect(server.calls).toEqual([]);
      expect(execs).toEqual([]);
      expect(attachRequests).toEqual([]);
      expect(clock.sleeps).toEqual([]);
    },
  );

  test("stops attaching when the token source fails after a clean stream end", async () => {
    const { server, execs, attachRequests } = execServer([started], []);
    const clock = new RecordingClock();
    const providerError = new Error("fixture token failure");
    let providerCalls = 0;
    const client = new SandRunClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      clock,
      token: () =>
        ++providerCalls === 1 ? "test-token" : Promise.reject(providerError),
    });
    const stream = client.execDevSession(execInput);
    expect((await stream.next()).value).toMatchObject({
      kind: ExecEventKind.Started,
    });
    expect((await stream.next()).value).toMatchObject({
      kind: ExecEventKind.Reattached,
    });
    const error = (await stream
      .next()
      .catch((error: unknown) => error)) as ConnectError;
    expect(error.cause).toBeInstanceOf(SandRunConfigError);
    expect((error.cause as SandRunConfigError).cause).toBe(providerError);
    expect(providerCalls).toBe(2);
    expect(server.calls).toHaveLength(1);
    expect(execs).toHaveLength(1);
    expect(attachRequests).toEqual([]);
    expect(clock.sleeps).toEqual([]);
  });

  test("continues with AttachDevSessionExec after each stream end and reports lost output", async () => {
    const { server, execs, attachRequests } = execServer(
      [started, out(0, "hel"), keepalive],
      [
        // The new stream repeats the start event and part of the delivered
        // output, then fails.
        [
          started,
          out(0, "hello"),
          err(0, "e"),
          keepalive,
          new ConnectError("relay restarting", Code.Unavailable),
        ],
        // The agent no longer keeps stdout bytes 5 to 8.
        [
          {
            case: "outputTruncated",
            value: { stdoutOffset: 8n, stderrOffset: 1n },
          },
          out(8, "!"),
          exited(0),
        ],
      ],
    );
    const clock = new RecordingClock();
    const events: string[] = [];
    for await (const event of client(server.fetch, clock).execDevSession({
      organizationId: ORG,
      devSessionId: SESSION,
      command: ["make", "test"],
      env: { CI: "1" },
      timeoutMs: 90_000,
    })) {
      events.push(describeEvent(event));
    }
    expect(events).toEqual([
      "started",
      "stdout 0 hel",
      "reattach 3 0",
      "stdout 3 lo",
      "stderr 0 e",
      "reattach 5 1",
      "lost stdout 5-8",
      "stdout 8 !",
      "exited 0",
    ]);
    expect(execs.length).toBe(1);
    const execId = execs[0]!.execId;
    expect(isUuidV7(execId)).toBe(true);
    expect(execs[0]!.command).toEqual(["make", "test"]);
    expect(execs[0]!.env).toEqual({ CI: "1" });
    expect(execs[0]!.timeout?.seconds).toBe(90n);
    expect(
      attachRequests.map((r) => [r.execId, r.stdoutOffset, r.stderrOffset]),
    ).toEqual([
      [execId, 3n, 0n],
      [execId, 5n, 1n],
    ]);
    // A clean end continues at once; a failure waits for the backoff.
    expect(clock.sleeps).toEqual([1000]);
  });

  test("collects the output and the outcome", async () => {
    const { server } = execServer(
      [started, out(0, "a"), err(0, "b")],
      [[out(1, "c"), exited(3)]],
    );
    const result = await client(server.fetch).runDevSessionExec({
      organizationId: ORG,
      devSessionId: SESSION,
      command: ["sh", "-c", "exit 3"],
    });
    expect(new TextDecoder().decode(result.stdout)).toBe("ac");
    expect(new TextDecoder().decode(result.stderr)).toBe("b");
    expect(result.lost).toEqual([]);
    expect(result.outcome).toEqual({
      kind: ExecEventKind.Exited,
      exitCode: 3n,
      signal: undefined,
      timedOut: false,
    });
    expect(isUuidV7(result.execId)).toBe(true);
  });

  test("reports a start failure and a sandbox end as outcomes", async () => {
    const failed = execServer(
      [
        {
          case: "failed",
          value: {
            reason: ExecFailureReason.PROGRAM_NOT_FOUND,
            message: "no such program",
          },
        },
      ],
      [],
    );
    const a = await client(failed.server.fetch).runDevSessionExec({
      organizationId: ORG,
      devSessionId: SESSION,
      command: ["missing"],
    });
    expect(a.outcome).toEqual({
      kind: ExecEventKind.Failed,
      reason: ExecFailureReason.PROGRAM_NOT_FOUND,
      message: "no such program",
    });
    const ended = execServer(
      [started, { case: "sandboxEnded", value: { reason: "MAX_TTL" } }],
      [],
    );
    const b = await client(ended.server.fetch).runDevSessionExec({
      organizationId: ORG,
      devSessionId: SESSION,
      command: ["sleep", "100"],
    });
    expect(b.outcome).toEqual({
      kind: ExecEventKind.SandboxEnded,
      reason: "MAX_TTL",
    });
  });

  test("never sends ExecDevSession twice and ends with its error when the command never started", async () => {
    const { server, execs, attachRequests } = execServer(
      [new ConnectError("overloaded", Code.Unavailable)],
      [[new ConnectError("unknown exec", Code.NotFound)]],
    );
    const error = await client(server.fetch)
      .runDevSessionExec({
        organizationId: ORG,
        devSessionId: SESSION,
        command: ["true"],
      })
      .catch((e: unknown) => e);
    expect((error as ConnectError).code).toBe(Code.Unavailable);
    expect(execs.length).toBe(1);
    expect(attachRequests.length).toBe(1);
  });

  test.each(["with", "without"])(
    "keeps the startup error after keepalives and attach NotFound %s an attach keepalive",
    async (attachKeepalive) => {
      const notFound = new ConnectError("unknown exec", Code.NotFound);
      const { server, execs, attachRequests } = execServer(
        [keepalive, new ConnectError("overloaded", Code.Unavailable)],
        [attachKeepalive === "with" ? [keepalive, notFound] : [notFound]],
      );
      const stream = client(server.fetch).execDevSession(execInput);
      expect(await stream.next()).toMatchObject({
        done: false,
        value: {
          kind: ExecEventKind.Reattached,
          stdoutOffset: 0n,
          stderrOffset: 0n,
        },
      });
      await expect(stream.next()).rejects.toMatchObject({
        code: Code.Unavailable,
        rawMessage: "overloaded",
      });
      expect(execs).toHaveLength(1);
      expect(attachRequests).toHaveLength(1);
      expect(attachRequests[0]).toMatchObject({
        execId: execs[0]!.execId,
        stdoutOffset: 0n,
        stderrOffset: 0n,
      });
    },
  );

  test("surfaces attach NotFound after a command starts", async () => {
    const { server, execs, attachRequests } = execServer(
      [started, keepalive, new ConnectError("overloaded", Code.Unavailable)],
      [[new ConnectError("unknown exec", Code.NotFound)]],
    );
    const stream = client(server.fetch).execDevSession(execInput);
    expect((await stream.next()).value).toMatchObject({
      kind: ExecEventKind.Started,
    });
    expect((await stream.next()).value).toMatchObject({
      kind: ExecEventKind.Reattached,
    });
    await expect(stream.next()).rejects.toMatchObject({
      code: Code.NotFound,
      rawMessage: "unknown exec",
    });
    expect(execs).toHaveLength(1);
    expect(attachRequests).toHaveLength(1);
  });

  test("backs off to the cap after replayed starts", async () => {
    const unavailable = new ConnectError("relay restarting", Code.Unavailable);
    const startThenFail = [started, unavailable];
    const { server, execs, attachRequests } = execServer(startThenFail, [
      ...Array.from({ length: 6 }, () => startThenFail),
      [exited(0)],
    ]);
    const clock = new RecordingClock();
    const events: string[] = [];
    for await (const event of client(server.fetch, clock).execDevSession(
      execInput,
    )) {
      events.push(describeEvent(event));
    }

    expect(clock.sleeps).toEqual([
      1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000,
    ]);
    expect(events).toEqual([
      "started",
      ...Array.from({ length: 7 }, () => "reattach 0 0"),
      "exited 0",
    ]);
    expect(execs).toHaveLength(1);
    expect(attachRequests).toHaveLength(7);
    for (const request of attachRequests) {
      expect(request).toMatchObject({
        execId: execs[0]!.execId,
        stdoutOffset: 0n,
        stderrOffset: 0n,
      });
    }
  });

  test("resets backoff when an attach replays Started and delivers new output", async () => {
    const unavailable = new ConnectError("relay restarting", Code.Unavailable);
    const { server, execs, attachRequests } = execServer(
      [started, unavailable],
      [
        [started, unavailable],
        [started, out(0, "hello"), err(0, "!"), unavailable],
        [exited(0)],
      ],
    );
    const clock = new RecordingClock();
    const events: string[] = [];
    for await (const event of client(server.fetch, clock).execDevSession(
      execInput,
    )) {
      events.push(describeEvent(event));
    }

    expect(clock.sleeps).toEqual([1_000, 2_000, 1_000]);
    expect(events).toEqual([
      "started",
      "reattach 0 0",
      "reattach 0 0",
      "stdout 0 hello",
      "stderr 0 !",
      "reattach 5 1",
      "exited 0",
    ]);
    expect(execs).toHaveLength(1);
    expect(
      attachRequests.map((r) => [r.execId, r.stdoutOffset, r.stderrOffset]),
    ).toEqual([
      [execs[0]!.execId, 0n, 0n],
      [execs[0]!.execId, 0n, 0n],
      [execs[0]!.execId, 5n, 1n],
    ]);
  });

  test.each([
    [2, [1_000, 2_000, 1_000]],
    [7, [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 1_000]],
  ])(
    "backs off after %i keepalive-only failures and resets after Started",
    async (keepaliveFailures: number, expectedSleeps: number[]) => {
      const unavailable = new ConnectError("overloaded", Code.Unavailable);
      const keepaliveOnly = [keepalive, unavailable];
      const { server, execs, attachRequests } = execServer(keepaliveOnly, [
        ...Array.from({ length: keepaliveFailures - 1 }, () => keepaliveOnly),
        [started, unavailable],
        [exited(0)],
      ]);
      const clock = new RecordingClock();
      const events: string[] = [];
      for await (const event of client(server.fetch, clock).execDevSession(
        execInput,
      )) {
        events.push(describeEvent(event));
      }
      expect(clock.sleeps).toEqual(expectedSleeps);
      expect(events).toEqual([
        ...Array.from({ length: keepaliveFailures }, () => "reattach 0 0"),
        "started",
        "reattach 0 0",
        "exited 0",
      ]);
      expect(execs).toHaveLength(1);
      expect(attachRequests).toHaveLength(keepaliveFailures + 1);
      for (const request of attachRequests) {
        expect(request).toMatchObject({
          execId: execs[0]!.execId,
          stdoutOffset: 0n,
          stderrOffset: 0n,
        });
      }
    },
  );

  test("ends with the server's error and keeps its code", async () => {
    const { server } = execServer(
      [new ConnectError("dev session is not running", Code.FailedPrecondition)],
      [],
    );
    const error = await client(server.fetch)
      .runDevSessionExec({
        organizationId: ORG,
        devSessionId: SESSION,
        command: ["true"],
      })
      .catch((e: unknown) => e);
    expect((error as ConnectError).code).toBe(Code.FailedPrecondition);
  });

  test("stops following when the signal aborts", async () => {
    const { server } = execServer([started, out(0, "x")], []);
    const abort = new AbortController();
    const events: ExecEvent[] = [];
    for await (const event of client(server.fetch).execDevSession({
      organizationId: ORG,
      devSessionId: SESSION,
      command: ["sleep", "1000"],
      signal: abort.signal,
    })) {
      events.push(event);
      if (event.kind === ExecEventKind.Stdout) abort.abort();
    }
    expect(events.map((e) => e.kind)).toEqual([
      ExecEventKind.Started,
      ExecEventKind.Stdout,
    ]);
    expect(OutputStream.Stdout).toBe("stdout");
  });

  test.each(["initial", "attach"])(
    "drops a buffered terminal event on the %s stream after cancellation",
    async (source) => {
      const abort = new AbortController();
      let closes = 0;
      let attaches = 0;
      function* bufferedEvents() {
        try {
          yield started;
          yield exited(0);
        } finally {
          closes++;
        }
      }
      const caller: ExecCaller = {
        async *execDevSession() {
          if (source === "initial") {
            for (const event of bufferedEvents()) {
              yield response(event);
            }
          }
        },
        async *attachDevSessionExec() {
          attaches++;
          for (const event of bufferedEvents()) {
            yield create(AttachDevSessionExecResponseSchema, {
              event: { event },
            });
          }
        },
      };
      const events = execDevSession(caller, {
        ...execInput,
        signal: abort.signal,
      });
      if (source === "attach") {
        expect((await events.next()).value).toMatchObject({
          kind: ExecEventKind.Reattached,
        });
      }
      expect((await events.next()).value).toMatchObject({
        kind: ExecEventKind.Started,
      });
      abort.abort();
      expect(await events.next()).toEqual({ done: true, value: undefined });
      expect(closes).toBe(1);
      expect(attaches).toBe(source === "attach" ? 1 : 0);
    },
  );

  test.each<{ name: string; event: EventInit }>([
    { name: "a chunk", event: out(2, "x") },
    {
      name: "a second loss event",
      event: {
        case: "outputTruncated",
        value: { stdoutOffset: 2n, stderrOffset: 3n },
      },
    },
  ])(
    "drops $name from the same response after cancellation",
    async ({ event }) => {
      const abort = new AbortController();
      let closed = false;
      const caller = execCaller(async function* () {
        try {
          yield response(event);
        } finally {
          closed = true;
        }
      });
      const events = execDevSession(caller, {
        ...execInput,
        signal: abort.signal,
      });
      expect((await events.next()).value).toEqual({
        kind: ExecEventKind.OutputLost,
        range: { stream: OutputStream.Stdout, start: 0n, end: 2n },
      });
      abort.abort();
      expect(await events.next()).toEqual({ done: true, value: undefined });
      expect(closed).toBe(true);
    },
  );

  test("rejects with the abort reason when a pending read returns a terminal event", async () => {
    const abort = new AbortController();
    const reason = new Error("exec canceled");
    const reading = Promise.withResolvers<void>();
    const terminal = Promise.withResolvers<void>();
    let closed = false;
    const caller = execCaller(async function* () {
      try {
        yield response(started);
        reading.resolve();
        await terminal.promise;
        yield response(exited(0));
      } finally {
        closed = true;
      }
    });
    const result = runDevSessionExec(caller, {
      ...execInput,
      signal: abort.signal,
    });
    await reading.promise;
    abort.abort(reason);
    terminal.resolve();
    await expect(result).rejects.toBe(reason);
    expect(closed).toBe(true);
  });

  test("keeps the terminal result when cancellation occurs during stream cleanup", async () => {
    const abort = new AbortController();
    const closing = Promise.withResolvers<void>();
    const cleanup = Promise.withResolvers<void>();
    const caller = execCaller(async function* () {
      try {
        yield response(exited(0));
      } finally {
        closing.resolve();
        await cleanup.promise;
      }
    });
    const result = runDevSessionExec(caller, {
      ...execInput,
      signal: abort.signal,
    });
    await closing.promise;
    abort.abort();
    cleanup.resolve();
    expect((await result).outcome).toMatchObject({
      kind: ExecEventKind.Exited,
      exitCode: 0n,
    });
  });

  test("starts no attach when the signal aborts after Reattached", async () => {
    const abort = new AbortController();
    let attaches = 0;
    const caller: ExecCaller = {
      async *execDevSession() {},
      async *attachDevSessionExec() {
        attaches++;
        yield create(AttachDevSessionExecResponseSchema, {
          event: { event: exited(0) },
        });
      },
    };
    const events = execDevSession(caller, {
      ...execInput,
      signal: abort.signal,
    });
    expect((await events.next()).value).toMatchObject({
      kind: ExecEventKind.Reattached,
    });
    abort.abort();
    expect(await events.next()).toEqual({ done: true, value: undefined });
    expect(attaches).toBe(0);
  });

  test("does not emit Reattached when the signal aborts as backoff ends", async () => {
    const abort = new AbortController();
    const sleeping = Promise.withResolvers<void>();
    const backoff = Promise.withResolvers<void>();
    const caller = execCaller(async function* () {
      throw new ConnectError("relay restarting", Code.Unavailable);
    });
    const clock = {
      now: () => 0,
      sleep() {
        sleeping.resolve();
        return backoff.promise;
      },
    };
    const events = execDevSession(
      caller,
      { ...execInput, signal: abort.signal },
      { clock },
    );
    const next = events.next();
    await sleeping.promise;
    backoff.resolve();
    abort.abort();
    expect(await next).toEqual({ done: true, value: undefined });
  });
});
