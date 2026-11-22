// SPDX-License-Identifier: Apache-2.0
import type { MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { Code, type CallOptions, ConnectError } from "@connectrpc/connect";
import type { ExecFailureReason } from "@delino/tide-protos/v1/common_pb";
import type {
  AttachDevSessionExecRequestSchema,
  AttachDevSessionExecResponse,
  DevSessionExecEvent,
  ExecDevSessionRequestSchema,
  ExecDevSessionResponse,
} from "@delino/tide-protos/v1/dev_session_pb";
import { abortedBy, backoffMs } from "./backoff.ts";
import { type Clock, systemClock } from "./clock.ts";
import { isRetryableStreamError, TideError } from "./errors.ts";
import { UuidV7Generator } from "./uuidv7.ts";

/** The output stream of an exec. */
export enum OutputStream {
  Stdout = "stdout",
  Stderr = "stderr",
}

/** Kinds of the events of `execDevSession`. */
export enum ExecEventKind {
  /** The command started. Sent at most once. */
  Started = "started",
  /** A chunk of standard output, in order and without overlap. */
  Stdout = "stdout",
  /** A chunk of standard error, in order and without overlap. */
  Stderr = "stderr",
  /**
   * Output that the helper never received: the server no longer kept it
   * (`output_truncated`) or a stream skipped it.
   */
  OutputLost = "output_lost",
  /**
   * The previous stream ended without a result and the helper continues
   * with `AttachDevSessionExec` from the next offsets.
   */
  Reattached = "reattached",
  /** The command ended. The last event. */
  Exited = "exited",
  /** The command could not start. The last event. */
  Failed = "failed",
  /** The sandbox ended before the result could be delivered. The last event. */
  SandboxEnded = "sandbox_ended",
}

/** A byte range `[start, end)` of one output stream. */
export interface LostRange {
  stream: OutputStream;
  start: bigint;
  end: bigint;
}

export type ExecEvent =
  | { kind: ExecEventKind.Started; execId: string }
  | { kind: ExecEventKind.Stdout; offset: bigint; data: Uint8Array }
  | { kind: ExecEventKind.Stderr; offset: bigint; data: Uint8Array }
  | { kind: ExecEventKind.OutputLost; range: LostRange }
  | {
      kind: ExecEventKind.Reattached;
      stdoutOffset: bigint;
      stderrOffset: bigint;
      /** The error that ended the previous stream; undefined for a clean end. */
      cause: unknown;
    }
  | {
      kind: ExecEventKind.Exited;
      /** Set when the command exited normally. */
      exitCode: bigint | undefined;
      /** Set when a signal ended the command. */
      signal: number | undefined;
      timedOut: boolean;
    }
  | { kind: ExecEventKind.Failed; reason: ExecFailureReason; message: string }
  | { kind: ExecEventKind.SandboxEnded; reason: string };

/** The part of the `DevSessionService` client that the exec helper uses. */
export interface ExecCaller {
  execDevSession(
    request: MessageInitShape<typeof ExecDevSessionRequestSchema>,
    options?: CallOptions,
  ): AsyncIterable<ExecDevSessionResponse>;
  attachDevSessionExec(
    request: MessageInitShape<typeof AttachDevSessionExecRequestSchema>,
    options?: CallOptions,
  ): AsyncIterable<AttachDevSessionExecResponse>;
}

export interface ExecInput {
  organizationId: string;
  devSessionId: string;
  /** Argument array; the first element is the program. */
  command: readonly string[];
  /** Environment of the command. The server neither stores nor logs it. */
  env?: Readonly<Record<string, string>>;
  /** An absolute guest path, or a path relative to the workspace path. */
  workingDirectory?: string;
  /** Stops the command after this many milliseconds. Absent means no timeout. */
  timeoutMs?: number;
  /** A UUID v7 that identifies the exec. Default: a new one. */
  execId?: string;
  /** Stops following the exec. The command keeps running in the guest. */
  signal?: AbortSignal;
}

export interface ExecHelperOptions {
  clock?: Clock;
  ids?: UuidV7Generator;
}

/**
 * Runs a command in a running dev session and streams its events.
 *
 * - It generates the `exec_id` (UUID v7) unless the input has one, and
 *   calls `ExecDevSession` exactly once, so it never runs a command twice.
 * - The server ends each stream before 14 minutes with an OK status and no
 *   final event. After such an end, and after a retryable failure, the
 *   helper continues with `AttachDevSessionExec` from the next stdout and
 *   stderr offsets. A clean end continues at once; failures wait with
 *   exponential backoff from 1 second to 30 seconds.
 * - It yields output in order and without duplicates, and reports output
 *   that it never received as `OutputLost`.
 * - A status that is the server's answer ends the iteration with that
 *   `ConnectError`. When `ExecDevSession` failed before any event and
 *   `AttachDevSessionExec` then answers `not_found`, the command never
 *   started, and the iteration ends with the error of `ExecDevSession`.
 */
export async function* execDevSession(
  devSessions: ExecCaller,
  input: ExecInput,
  options: ExecHelperOptions = {},
): AsyncGenerator<ExecEvent, void, undefined> {
  const clock = options.clock ?? systemClock;
  const ids = options.ids ?? new UuidV7Generator(() => clock.now());
  const execId = input.execId ?? ids.next();
  const signal = input.signal;
  let stdoutOffset = 0n;
  let stderrOffset = 0n;
  let started = false;
  let anyEvent = false;
  let execError: unknown;
  let failures = 0;

  function* handle(
    event: DevSessionExecEvent["event"],
  ): Generator<ExecEvent, boolean, undefined> {
    switch (event.case) {
      case "started":
        if (!started) {
          started = true;
          yield { kind: ExecEventKind.Started, execId };
        }
        return false;
      case "outputTruncated": {
        const first = event.value;
        if (first.stdoutOffset > stdoutOffset) {
          yield lost(OutputStream.Stdout, stdoutOffset, first.stdoutOffset);
          stdoutOffset = first.stdoutOffset;
        }
        if (first.stderrOffset > stderrOffset) {
          yield lost(OutputStream.Stderr, stderrOffset, first.stderrOffset);
          stderrOffset = first.stderrOffset;
        }
        return false;
      }
      case "stdout":
      case "stderr": {
        const stream =
          event.case === "stdout" ? OutputStream.Stdout : OutputStream.Stderr;
        const expected =
          stream === OutputStream.Stdout ? stdoutOffset : stderrOffset;
        const { offset, data } = event.value;
        const end = offset + BigInt(data.length);
        if (end <= expected) {
          // A chunk that an earlier stream already delivered.
          return false;
        }
        if (offset > expected) {
          yield lost(stream, expected, offset);
        }
        const from = offset < expected ? expected : offset;
        const chunk = data.subarray(Number(from - offset));
        if (stream === OutputStream.Stdout) {
          stdoutOffset = end;
          yield { kind: ExecEventKind.Stdout, offset: from, data: chunk };
        } else {
          stderrOffset = end;
          yield { kind: ExecEventKind.Stderr, offset: from, data: chunk };
        }
        return false;
      }
      case "exited": {
        const status = event.value.status;
        yield {
          kind: ExecEventKind.Exited,
          exitCode: status.case === "exitCode" ? status.value : undefined,
          signal: status.case === "signal" ? status.value : undefined,
          timedOut: event.value.timedOut,
        };
        return true;
      }
      case "failed":
        yield {
          kind: ExecEventKind.Failed,
          reason: event.value.reason,
          message: event.value.message,
        };
        return true;
      case "sandboxEnded":
        yield { kind: ExecEventKind.SandboxEnded, reason: event.value.reason };
        return true;
      default:
        // Keepalives and events of a newer schema carry no state.
        return false;
    }
  }

  for (let attempt = 0; ; attempt++) {
    let cause: unknown;
    let progressed = false;
    try {
      const stream: AsyncIterable<
        ExecDevSessionResponse | AttachDevSessionExecResponse
      > =
        attempt === 0
          ? devSessions.execDevSession(
              {
                organizationId: input.organizationId,
                devSessionId: input.devSessionId,
                execId,
                command: [...input.command],
                env: { ...input.env },
                workingDirectory: input.workingDirectory ?? "",
                timeout:
                  input.timeoutMs === undefined
                    ? undefined
                    : durationFromMs(input.timeoutMs),
              },
              { signal },
            )
          : devSessions.attachDevSessionExec(
              {
                organizationId: input.organizationId,
                devSessionId: input.devSessionId,
                execId,
                stdoutOffset,
                stderrOffset,
              },
              { signal },
            );
      for await (const message of stream) {
        if (message.event === undefined) {
          continue;
        }
        anyEvent = true;
        progressed = true;
        if (yield* handle(message.event.event)) {
          return;
        }
      }
    } catch (error) {
      if (abortedBy(signal)) {
        return;
      }
      if (
        attempt > 0 &&
        !anyEvent &&
        execError !== undefined &&
        error instanceof ConnectError &&
        error.code === Code.NotFound
      ) {
        throw execError;
      }
      if (!isRetryableStreamError(error)) {
        throw error;
      }
      if (attempt === 0 && !anyEvent) {
        execError = error;
      }
      cause = error;
    }
    if (abortedBy(signal)) {
      return;
    }
    if (cause === undefined) {
      failures = 0;
    } else {
      failures = progressed ? 1 : failures + 1;
      try {
        await clock.sleep(backoffMs(failures - 1), signal);
      } catch (error) {
        if (abortedBy(signal)) {
          return;
        }
        throw error;
      }
    }
    yield {
      kind: ExecEventKind.Reattached,
      stdoutOffset,
      stderrOffset,
      cause,
    };
  }
}

function lost(stream: OutputStream, start: bigint, end: bigint): ExecEvent {
  return { kind: ExecEventKind.OutputLost, range: { stream, start, end } };
}

/** How an exec ended. */
export type ExecOutcome =
  | Extract<ExecEvent, { kind: ExecEventKind.Exited }>
  | Extract<ExecEvent, { kind: ExecEventKind.Failed }>
  | Extract<ExecEvent, { kind: ExecEventKind.SandboxEnded }>;

export interface ExecResult {
  execId: string;
  stdout: Uint8Array;
  stderr: Uint8Array;
  /** Output that the helper never received. */
  lost: LostRange[];
  outcome: ExecOutcome;
}

/**
 * Runs a command with `execDevSession` and collects its output and outcome.
 * Output is kept in memory; use `execDevSession` for large output. It
 * fails with the `ConnectError` of the server, or with the abort reason
 * when the signal aborts before the command ends.
 */
export async function runDevSessionExec(
  devSessions: ExecCaller,
  input: ExecInput,
  options: ExecHelperOptions = {},
): Promise<ExecResult> {
  const clock = options.clock ?? systemClock;
  const ids = options.ids ?? new UuidV7Generator(() => clock.now());
  const execId = input.execId ?? ids.next();
  const stdout: Uint8Array[] = [];
  const stderr: Uint8Array[] = [];
  const lostRanges: LostRange[] = [];
  let outcome: ExecOutcome | undefined;
  for await (const event of execDevSession(
    devSessions,
    { ...input, execId },
    { clock, ids },
  )) {
    switch (event.kind) {
      case ExecEventKind.Stdout:
        stdout.push(event.data);
        break;
      case ExecEventKind.Stderr:
        stderr.push(event.data);
        break;
      case ExecEventKind.OutputLost:
        lostRanges.push(event.range);
        break;
      case ExecEventKind.Exited:
      case ExecEventKind.Failed:
      case ExecEventKind.SandboxEnded:
        outcome = event;
        break;
      default:
        break;
    }
  }
  if (outcome === undefined) {
    input.signal?.throwIfAborted();
    throw new TideError("the exec ended without a result");
  }
  return {
    execId,
    stdout: concat(stdout),
    stderr: concat(stderr),
    lost: lostRanges,
    outcome,
  };
}

function concat(chunks: readonly Uint8Array[]): Uint8Array {
  let size = 0;
  for (const chunk of chunks) size += chunk.length;
  const out = new Uint8Array(size);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.length;
  }
  return out;
}
