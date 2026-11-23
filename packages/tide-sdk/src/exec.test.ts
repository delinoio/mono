// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { ExecFailureReason } from "@delino/tide-protos/v1/common_pb";
import {
  type AttachDevSessionExecRequest,
  type DevSessionExecEvent,
  DevSessionService,
  type ExecDevSessionRequest,
} from "@delino/tide-protos/v1/dev_session_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { DevSessionExecEventSchema } from "@delino/tide-protos/v1/dev_session_pb";
import { TideClient } from "./client.ts";
import { type ExecEvent, ExecEventKind, OutputStream } from "./exec.ts";
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
const exited = (code: number): EventInit => ({
  case: "exited",
  value: { status: { case: "exitCode", value: BigInt(code) } },
});

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
  return new TideClient({
    origin: FAKE_ORIGIN,
    fetch,
    token: "tide_personal_key",
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
  test("continues with AttachDevSessionExec after each stream end and reports lost output", async () => {
    const { server, execs, attachRequests } = execServer(
      [started, out(0, "hel"), { case: "keepalive", value: {} }],
      [
        // The new stream repeats the start event and part of the delivered
        // output, then fails.
        [
          started,
          out(0, "hello"),
          err(0, "e"),
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
});
