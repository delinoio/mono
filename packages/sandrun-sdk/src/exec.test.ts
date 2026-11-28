// SPDX-License-Identifier: Apache-2.0
import { create, fromBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, rs, test } from "@rstest/core";
import { ExecFailureReason } from "@delino/sandrun-protos/v1/common_pb";
import {
  type AttachDevSessionExecRequest,
  type AttachDevSessionExecResponse,
  AttachDevSessionExecResponseSchema,
  type DevSessionExecEvent,
  type DevSessionExecEventSchema,
  DevSessionService,
  type ExecDevSessionRequest,
  type ExecDevSessionResponse,
  ExecDevSessionResponseSchema,
} from "@delino/sandrun-protos/v1/dev_session_pb";
import { SandRunClient } from "./client.ts";
import { SandRunConfigError } from "./errors.ts";
import {
  type ExecCaller,
  type ExecEvent,
  type ExecOutcome,
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
const EXEC = "0193a000-0000-7000-8000-0000000000e1";

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
const truncated = (stdoutOffset: bigint, stderrOffset: bigint): EventInit => ({
  case: "outputTruncated",
  value: { stdoutOffset, stderrOffset },
});
const started: EventInit = { case: "started", value: { execId: "x" } };
const keepalive: EventInit = { case: "keepalive", value: {} };
const exited = (code: number): EventInit => ({
  case: "exited",
  value: { status: { case: "exitCode", value: BigInt(code) } },
});

// Response field 1 wraps an exec event with unknown inner field 9.
const futureEvent = Uint8Array.of(0x0a, 0x02, 0x4a, 0x00);

const execInput = {
  organizationId: ORG,
  devSessionId: SESSION,
  command: ["true"],
};

function response(event: EventInit) {
  return create(ExecDevSessionResponseSchema, { event: { event } });
}

function scriptedExecCaller(
  exec: (ExecDevSessionResponse | ConnectError)[],
  attaches: (AttachDevSessionExecResponse | ConnectError)[][],
) {
  async function* play<T>(script: (T | ConnectError)[]) {
    for (const step of script) {
      if (step instanceof ConnectError) {
        throw step;
      }
      yield step;
    }
  }

  let attachIndex = 0;
  return {
    execDevSession: rs.fn(
      (_request: Parameters<ExecCaller["execDevSession"]>[0]) => play(exec),
    ),
    attachDevSessionExec: rs.fn(
      (_request: Parameters<ExecCaller["attachDevSessionExec"]>[0]) =>
        play(attaches[attachIndex++]!),
    ),
  } satisfies ExecCaller;
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
 * A dev session server that plays `exec` and consumes one script per attach.
 * An attach without a script fails with Code.Internal. Each script ends the
 * stream with its ConnectError or, when exhausted, an OK status with no final
 * event, as the server does before 14 minutes.
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
        const attachNumber = attachRequests.length;
        const script = attaches[attachNumber - 1];
        if (script === undefined) {
          throw new ConnectError(
            `No script for attach request ${attachNumber} (${attaches.length} scripts supplied)`,
            Code.Internal,
          );
        }
        return play(script);
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
  test.each([undefined, EXEC])(
    "handles a pre-aborted signal before resolving exec ID %s",
    async (execId) => {
      const abort = new AbortController();
      const reason = new Error("exec canceled");
      abort.abort(reason);
      const now = rs.fn(() => 2 ** 48);
      const sleep = rs.fn(async () => {});
      const rpc = rs.fn(() => {
        throw new ConnectError("unexpected RPC", Code.FailedPrecondition);
      });
      const caller: ExecCaller = {
        execDevSession: rpc,
        attachDevSessionExec: rpc,
      };
      const input = { ...execInput, execId, signal: abort.signal };
      const options = { clock: { now, sleep } };

      expect(await execDevSession(caller, input, options).next()).toEqual({
        done: true,
        value: undefined,
      });
      await expect(runDevSessionExec(caller, input, options)).rejects.toBe(
        reason,
      );
      expect(now).not.toHaveBeenCalled();
      expect(sleep).not.toHaveBeenCalled();
      expect(rpc).not.toHaveBeenCalled();
    },
  );

  test("surfaces ID generation errors for active calls with no exec ID", async () => {
    const now = rs.fn(() => 2 ** 48);
    const sleep = rs.fn(async () => {});
    const rpc = rs.fn(() => {
      throw new ConnectError("unexpected RPC", Code.FailedPrecondition);
    });
    const caller: ExecCaller = {
      execDevSession: rpc,
      attachDevSessionExec: rpc,
    };
    const input = { ...execInput, signal: new AbortController().signal };
    const options = { clock: { now, sleep } };
    const message = "UUID v7 timestamp exceeds the 48-bit limit";

    await expect(execDevSession(caller, input, options).next()).rejects.toThrow(
      message,
    );
    await expect(runDevSessionExec(caller, input, options)).rejects.toThrow(
      message,
    );
    expect(now).toHaveBeenCalledTimes(2);
    expect(sleep).not.toHaveBeenCalled();
    expect(rpc).not.toHaveBeenCalled();
  });

  test.each([Number.NaN, Infinity, -Infinity])(
    "rejects a non-finite timeout %s before dispatch or retry",
    async (timeoutMs) => {
      const clock = new RecordingClock();
      const start = rs.fn(() => {
        throw new ConnectError("unexpected exec", Code.FailedPrecondition);
      });
      const attach = rs.fn(() => {
        throw new ConnectError("unknown exec", Code.NotFound);
      });
      const caller: ExecCaller = {
        execDevSession: start,
        attachDevSessionExec: attach,
      };
      const input = { ...execInput, execId: EXEC, timeoutMs };

      await expect(
        execDevSession(caller, input, { clock }).next(),
      ).rejects.toThrow(RangeError);
      await expect(runDevSessionExec(caller, input, { clock })).rejects.toThrow(
        RangeError,
      );
      expect(start).not.toHaveBeenCalled();
      expect(attach).not.toHaveBeenCalled();
      expect(clock.sleeps).toEqual([]);
    },
  );

  test.each([
    { timeoutMs: 1000.1, timeout: { seconds: 1n, nanos: 100_000 } },
    { timeoutMs: 999.9999996, timeout: { seconds: 1n, nanos: 0 } },
    { timeoutMs: 1000.5, timeout: { seconds: 1n, nanos: 500_000 } },
    { timeoutMs: 90_000, timeout: { seconds: 90n, nanos: 0 } },
    { timeoutMs: 0.0000004, timeout: { seconds: 0n, nanos: 0 } },
    { timeoutMs: 0.0000005, timeout: { seconds: 0n, nanos: 1 } },
    { timeoutMs: undefined, timeout: undefined },
  ])(
    "encodes timeout $timeoutMs on the wire",
    async ({ timeoutMs, timeout }) => {
      const { server, execs, attachRequests } = execServer([exited(0)], []);
      const result = await client(server.fetch).runDevSessionExec({
        ...execInput,
        execId: EXEC,
        timeoutMs,
      });

      expect(result.outcome).toMatchObject({
        kind: ExecEventKind.Exited,
        exitCode: 0n,
      });
      expect(server.calls).toHaveLength(1);
      expect(execs).toHaveLength(1);
      expect(execs[0]!.timeout).toEqual(
        timeout === undefined
          ? undefined
          : { $typeName: "google.protobuf.Duration", ...timeout },
      );
      expect(attachRequests).toEqual([]);
    },
  );

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

  test("stops at the first attach request without a script", async () => {
    const { server, execs, attachRequests } = execServer([started], []);
    const clock = new RecordingClock();
    const stream = client(server.fetch, clock).execDevSession({
      ...execInput,
      execId: EXEC,
    });

    expect((await stream.next()).value).toMatchObject({
      kind: ExecEventKind.Started,
    });
    expect((await stream.next()).value).toMatchObject({
      kind: ExecEventKind.Reattached,
    });
    await expect(stream.next()).rejects.toMatchObject({
      code: Code.Internal,
      rawMessage: "No script for attach request 1 (0 scripts supplied)",
      cause: undefined,
    });
    expect(await stream.next()).toEqual({ done: true, value: undefined });
    expect(execs).toHaveLength(1);
    expect(attachRequests).toHaveLength(1);
    expect(attachRequests[0]).toMatchObject({
      execId: EXEC,
      stdoutOffset: 0n,
      stderrOffset: 0n,
    });
    expect(clock.sleeps).toEqual([]);
  });

  test("continues after an explicitly empty attach script", async () => {
    const { server, execs, attachRequests } = execServer(
      [started],
      [[], [exited(0)]],
    );
    const clock = new RecordingClock();
    const events: string[] = [];
    for await (const event of client(server.fetch, clock).execDevSession(
      execInput,
    )) {
      events.push(describeEvent(event));
    }

    expect(events).toEqual([
      "started",
      "reattach 0 0",
      "reattach 0 0",
      "exited 0",
    ]);
    expect(execs).toHaveLength(1);
    expect(attachRequests).toHaveLength(2);
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

  test.each([
    { source: "initial", finish: "next" },
    { source: "initial", finish: "return" },
    { source: "attach", finish: "next" },
    { source: "attach", finish: "return" },
  ])(
    "completes with $finish after terminal cleanup rejects on the $source stream",
    async ({ source, finish }) => {
      let closes = 0;
      let attaches = 0;
      const clock = new RecordingClock();
      async function* terminalStream<T>(wrap: (event: EventInit) => T) {
        try {
          yield wrap(out(0, "x"));
          yield wrap(exited(0));
        } finally {
          closes++;
          throw new ConnectError("cleanup failed", Code.Unavailable);
        }
      }
      const caller: ExecCaller = {
        async *execDevSession() {
          if (source === "initial") {
            yield* terminalStream(response);
          }
        },
        async *attachDevSessionExec() {
          attaches++;
          if (source === "attach" && attaches === 1) {
            yield* terminalStream((event) =>
              create(AttachDevSessionExecResponseSchema, { event: { event } }),
            );
          } else {
            yield create(AttachDevSessionExecResponseSchema, {
              event: { event: exited(99) },
            });
          }
        },
      };
      const events = execDevSession(caller, execInput, { clock });
      if (source === "attach") {
        expect((await events.next()).value).toMatchObject({
          kind: ExecEventKind.Reattached,
          cause: undefined,
        });
      }
      expect((await events.next()).value).toMatchObject({
        kind: ExecEventKind.Stdout,
        data: text.encode("x"),
      });
      expect((await events.next()).value).toMatchObject({
        kind: ExecEventKind.Exited,
        exitCode: 0n,
      });
      const completion =
        finish === "return"
          ? await events.return(undefined)
          : await events.next();
      expect(completion).toEqual({ done: true, value: undefined });
      expect(await events.next()).toEqual({ done: true, value: undefined });
      expect(closes).toBe(1);
      expect(attaches).toBe(source === "attach" ? 1 : 0);
      expect(clock.sleeps).toEqual([]);
    },
  );

  test("propagates an error thrown by the consumer after a terminal event", async () => {
    const consumerError = new ConnectError(
      "consumer failed",
      Code.PermissionDenied,
    );
    const caller = execCaller(async function* () {
      try {
        yield response(exited(0));
      } finally {
        throw new ConnectError("cleanup failed", Code.Unavailable);
      }
    });
    const events = execDevSession(caller, execInput);
    expect((await events.next()).value).toMatchObject({
      kind: ExecEventKind.Exited,
    });
    await expect(events.throw(consumerError)).rejects.toBe(consumerError);
  });

  test.each<{ event: EventInit; outcome: ExecOutcome }>([
    {
      event: exited(0),
      outcome: {
        kind: ExecEventKind.Exited,
        exitCode: 0n,
        signal: undefined,
        timedOut: false,
      },
    },
    {
      event: {
        case: "failed",
        value: {
          reason: ExecFailureReason.PROGRAM_NOT_FOUND,
          message: "no such program",
        },
      },
      outcome: {
        kind: ExecEventKind.Failed,
        reason: ExecFailureReason.PROGRAM_NOT_FOUND,
        message: "no such program",
      },
    },
    {
      event: { case: "sandboxEnded", value: { reason: "MAX_TTL" } },
      outcome: { kind: ExecEventKind.SandboxEnded, reason: "MAX_TTL" },
    },
  ])(
    "keeps the $outcome.kind outcome and output when terminal cleanup rejects",
    async ({ event, outcome }) => {
      const clock = new RecordingClock();
      let closes = 0;
      const caller = execCaller(async function* () {
        try {
          yield response(out(0, "a"));
          yield response(err(0, "b"));
          yield response(event);
        } finally {
          closes++;
          throw new ConnectError("cleanup denied", Code.PermissionDenied);
        }
      });
      const result = await runDevSessionExec(caller, execInput, { clock });
      expect(result.outcome).toEqual(outcome);
      expect(result.stdout).toEqual(text.encode("a"));
      expect(result.stderr).toEqual(text.encode("b"));
      expect(closes).toBe(1);
      expect(clock.sleeps).toEqual([]);
    },
  );

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
    const clock = new RecordingClock();
    const error = await client(server.fetch, clock)
      .runDevSessionExec({
        ...execInput,
        timeoutMs: 90_000,
      })
      .catch((e: unknown) => e);
    expect((error as ConnectError).code).toBe(Code.Unavailable);
    expect(execs.length).toBe(1);
    expect(execs[0]!.timeout).toMatchObject({ seconds: 90n, nanos: 0 });
    expect(attachRequests.length).toBe(1);
    expect(attachRequests[0]!.execId).toBe(execs[0]!.execId);
    expect(clock.sleeps).toEqual([1_000]);
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

  test.each(["exec", "attach", "attach after Started"] as const)(
    "reports the expected error after an unknown event on %s",
    async (placement) => {
      const startupError = new ConnectError(
        "startup failure",
        Code.Unavailable,
      );
      const attachError = new ConnectError("attach missing", Code.NotFound);
      const unknownExec = fromBinary(ExecDevSessionResponseSchema, futureEvent);
      const unknownAttach = fromBinary(
        AttachDevSessionExecResponseSchema,
        futureEvent,
      );
      const startedBeforeAttach = placement === "attach after Started";
      const execEvents = {
        exec: [unknownExec],
        attach: [],
        "attach after Started": [response(started)],
      }[placement];
      const attachEvents = placement === "exec" ? [] : [unknownAttach];
      const makeCaller = () =>
        scriptedExecCaller(
          [...execEvents, startupError],
          [[...attachEvents, attachError]],
        );
      const caller = makeCaller();
      const clock = new RecordingClock();
      const stream = execDevSession(caller, execInput, { clock });
      if (startedBeforeAttach) {
        expect((await stream.next()).value).toMatchObject({
          kind: ExecEventKind.Started,
        });
      }
      expect(await stream.next()).toEqual({
        done: false,
        value: {
          kind: ExecEventKind.Reattached,
          stdoutOffset: 0n,
          stderrOffset: 0n,
          cause: startupError,
        },
      });
      await expect(stream.next()).rejects.toBe(
        startedBeforeAttach ? attachError : startupError,
      );
      expect(caller.execDevSession).toHaveBeenCalledTimes(1);
      expect(caller.attachDevSessionExec).toHaveBeenCalledTimes(1);
      expect(caller.attachDevSessionExec.mock.calls[0]![0]).toMatchObject({
        execId: caller.execDevSession.mock.calls[0]![0].execId,
        stdoutOffset: 0n,
        stderrOffset: 0n,
      });
      expect(clock.sleeps).toEqual([1_000]);

      if (placement === "attach") {
        await expect(
          runDevSessionExec(makeCaller(), execInput, {
            clock: new RecordingClock(),
          }),
        ).rejects.toBe(startupError);
      }
    },
  );

  test("backs off to the cap after unknown events and resets after the first Started", async () => {
    const unavailable = new ConnectError("relay restarting", Code.Unavailable);
    const unknownAttach = fromBinary(
      AttachDevSessionExecResponseSchema,
      futureEvent,
    );
    const attachResponse = (event: EventInit) =>
      create(AttachDevSessionExecResponseSchema, { event: { event } });
    const caller = scriptedExecCaller(
      [unavailable],
      [
        ...Array.from({ length: 6 }, () => [unknownAttach, unavailable]),
        [attachResponse(started), unavailable],
        [attachResponse(exited(0))],
      ],
    );
    const clock = new RecordingClock();
    const events: string[] = [];
    for await (const event of execDevSession(caller, execInput, { clock })) {
      events.push(describeEvent(event));
    }

    expect(clock.sleeps).toEqual([
      1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 1_000,
    ]);
    expect(events).toEqual([
      ...Array.from({ length: 7 }, () => "reattach 0 0"),
      "started",
      "reattach 0 0",
      "exited 0",
    ]);
    expect(caller.execDevSession).toHaveBeenCalledTimes(1);
    expect(caller.attachDevSessionExec).toHaveBeenCalledTimes(8);
    const execId = caller.execDevSession.mock.calls[0]![0].execId;
    for (const [request] of caller.attachDevSessionExec.mock.calls) {
      expect(request).toMatchObject({
        execId,
        stdoutOffset: 0n,
        stderrOffset: 0n,
      });
    }
  });

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

  test.each([out(0, ""), err(0, ""), truncated(0n, 0n)])(
    "surfaces attach NotFound after a non-progressing $case event",
    async (event) => {
      const unavailable = new ConnectError(
        "relay restarting",
        Code.Unavailable,
      );
      const notFound = new ConnectError("unknown exec", Code.NotFound);
      const { server } = execServer([unavailable], [[event, notFound]]);
      const clock = new RecordingClock();

      await expect(
        client(server.fetch, clock).runDevSessionExec(execInput),
      ).rejects.toMatchObject({
        code: Code.NotFound,
        rawMessage: "unknown exec",
      });
      expect(clock.sleeps).toEqual([1_000]);
    },
  );

  test.each([
    {
      name: "two replayed output chunks",
      replays: [out(0, "abc"), err(0, "XY")],
    },
    {
      name: "replayed output through the cap",
      replays: [
        out(0, "abc"),
        err(0, "XY"),
        out(0, "ab"),
        err(1, "Y"),
        out(3, ""),
        err(0, "XY"),
      ],
    },
    {
      name: "no-op truncation through the cap",
      replays: [
        truncated(3n, 2n),
        truncated(1n, 0n),
        truncated(3n, 1n),
        truncated(0n, 2n),
        truncated(2n, 1n),
        truncated(0n, 0n),
      ],
    },
  ])("preserves backoff after $name", async ({ replays }) => {
    const unavailable = new ConnectError("relay restarting", Code.Unavailable);
    const { server, execs, attachRequests } = execServer(
      [started, out(0, "abc"), err(0, "XY"), unavailable],
      [...replays.map((event) => [event, unavailable]), [exited(0)]],
    );
    const clock = new RecordingClock();
    const events: string[] = [];
    for await (const event of client(server.fetch, clock).execDevSession(
      execInput,
    )) {
      events.push(describeEvent(event));
    }

    const attachCount = replays.length + 1;
    expect(clock.sleeps).toEqual(
      [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000].slice(
        0,
        attachCount,
      ),
    );
    expect(events).toEqual([
      "started",
      "stdout 0 abc",
      "stderr 0 XY",
      ...Array.from({ length: attachCount }, () => "reattach 3 2"),
      "exited 0",
    ]);
    expect(execs).toHaveLength(1);
    expect(attachRequests).toHaveLength(attachCount);
    for (const request of attachRequests) {
      expect(request).toMatchObject({
        execId: execs[0]!.execId,
        stdoutOffset: 3n,
        stderrOffset: 2n,
      });
    }
  });

  test.each([
    {
      name: "partially overlapping output",
      event: out(2, "cd"),
      delivered: "stdout 3 d",
      stdoutOffset: 4n,
    },
    {
      name: "advancing truncation",
      event: truncated(5n, 1n),
      delivered: "lost stdout 3-5",
      stdoutOffset: 5n,
    },
  ])(
    "resets backoff after $name",
    async ({ event, delivered, stdoutOffset }) => {
      const unavailable = new ConnectError(
        "relay restarting",
        Code.Unavailable,
      );
      const { server, execs, attachRequests } = execServer(
        [started, out(0, "abc"), err(0, "XY"), unavailable],
        [[out(0, "abc"), unavailable], [event, unavailable], [exited(0)]],
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
        "stdout 0 abc",
        "stderr 0 XY",
        "reattach 3 2",
        "reattach 3 2",
        delivered,
        `reattach ${stdoutOffset} 2`,
        "exited 0",
      ]);
      expect(execs).toHaveLength(1);
      const execId = execs[0]!.execId;
      expect(
        attachRequests.map((r) => [r.execId, r.stdoutOffset, r.stderrOffset]),
      ).toEqual([
        [execId, 3n, 2n],
        [execId, 3n, 2n],
        [execId, stdoutOffset, 2n],
      ]);
    },
  );

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
