// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";

import { DataPathError, isInterruption } from "../errors.ts";
import {
  RelayOperationKind,
  SandboxEndReason,
} from "../gen/delino/ownsand/v1/common_pb.ts";
import {
  AttachExecRequestSchema,
  type ExecRequest,
  OutputStream,
} from "../gen/delino/ownsand/v1/datapath_pb.ts";
import { OPERATION_ID_MAX_AGE_MS, uuidV7Time } from "../ids.ts";
import { LogEvent } from "../log.ts";
import type { RelayContext } from "./context.ts";
import { OperationGrant, SERVER_STREAM_NEED_MS } from "./grant.ts";
import {
  backoffMs,
  isRetryableRelayFailure,
  RecoveryWindow,
} from "./recovery.ts";
import { watchedStream } from "./stream.ts";
import { SandboxWaitResult, waitForRunning } from "./wait.ts";

/** Kinds of exec stream events that the caller receives. */
export enum ExecEventKind {
  STARTED = "started",
  STDOUT = "stdout",
  STDERR = "stderr",
  /** Output was lost; the result is incomplete. */
  TRUNCATED = "truncated",
}

/** A half-open byte range `[from, to)` of one output stream. */
export interface OffsetRange {
  from: bigint;
  to: bigint;
}

export type ExecStreamEvent =
  | { kind: ExecEventKind.STARTED; execId: string }
  | {
      kind: ExecEventKind.STDOUT | ExecEventKind.STDERR;
      offset: bigint;
      data: Uint8Array;
    }
  | {
      kind: ExecEventKind.TRUNCATED;
      stdout?: OffsetRange;
      stderr?: OffsetRange;
    };

/** Kinds of exec results. */
export enum ExecResultKind {
  /** The command ended with an exit code or a signal. */
  EXITED = "exited",
  /** The command could not start. */
  FAILED = "failed",
  /** The sandbox ended before the exec result could be delivered. */
  SANDBOX_ENDED = "sandbox_ended",
}

interface ExecResultCommon {
  execId: string;
  /** False when the stream reported lost output. */
  complete: boolean;
}

export type ExecResult = ExecResultCommon &
  (
    | {
        kind: ExecResultKind.EXITED;
        exitCode?: bigint;
        signal?: number;
        timedOut: boolean;
      }
    | { kind: ExecResultKind.FAILED; reason: string }
    | { kind: ExecResultKind.SANDBOX_ENDED; reason: SandboxEndReason }
  );

type DistributiveOmit<T, K extends PropertyKey> = T extends unknown
  ? Omit<T, K>
  : never;

enum NextRequest {
  EXEC = "exec",
  ATTACH = "attach",
}

/**
 * Runs or attaches to one exec over the relay and applies the continuation,
 * rotation and recovery rules of the SDK contract. With `request`, the exec is
 * started (or continued) under an `EXEC` grant; without it, the operation is
 * an attach-only `ATTACH_EXEC` continuation of `execId`.
 */
export async function* relayExec(
  ctx: RelayContext,
  execId: string,
  request: ExecRequest | undefined,
  digest: Uint8Array | undefined,
  signal: AbortSignal,
): AsyncGenerator<ExecStreamEvent, ExecResult, undefined> {
  const grant = new OperationGrant(
    ctx,
    request ? RelayOperationKind.EXEC : RelayOperationKind.ATTACH_EXEC,
    execId,
    digest,
  );
  let next = request ? NextRequest.EXEC : NextRequest.ATTACH;
  let started = false;
  let complete = true;
  let stdoutOffset = 0n;
  let stderrOffset = 0n;
  let window: RecoveryWindow | undefined;
  let lastError: unknown;

  const result = (
    r: DistributiveOmit<ExecResult, "execId" | "complete">,
  ): ExecResult => ({ ...r, execId, complete }) as ExecResult;

  for (;;) {
    signal.throwIfAborted();
    const grantId = await grant.acquire(SERVER_STREAM_NEED_MS, signal, window);
    let accepted = false;
    const sending = next;
    try {
      const stream = watchedStream(ctx, grant, signal, (call) =>
        sending === NextRequest.EXEC
          ? ctx.exec(grantId, request!, call)
          : ctx.attachExec(
              grantId,
              create(AttachExecRequestSchema, {
                organizationId: ctx.organizationId,
                sandboxId: ctx.sandboxId,
                execId,
                stdoutOffset,
                stderrOffset,
              }),
              call,
            ),
      );
      let paused = false;
      for await (const ev of stream) {
        const e = ev.event;
        switch (e.case) {
          case "accepted":
            accepted = true;
            window?.pause();
            break;
          case "keepalive":
            break;
          case "started":
            if (!started) {
              started = true;
              yield { kind: ExecEventKind.STARTED, execId };
            }
            break;
          case "outputTruncated": {
            const t: { stdout?: OffsetRange; stderr?: OffsetRange } = {};
            if (e.value.firstStdoutOffset > stdoutOffset) {
              t.stdout = { from: stdoutOffset, to: e.value.firstStdoutOffset };
              stdoutOffset = e.value.firstStdoutOffset;
            }
            if (e.value.firstStderrOffset > stderrOffset) {
              t.stderr = { from: stderrOffset, to: e.value.firstStderrOffset };
              stderrOffset = e.value.firstStderrOffset;
            }
            if (t.stdout || t.stderr) {
              complete = false;
              yield { kind: ExecEventKind.TRUNCATED, ...t };
            }
            break;
          }
          case "output": {
            const isErr = e.value.stream === OutputStream.STDERR;
            const current = isErr ? stderrOffset : stdoutOffset;
            const start = e.value.offset;
            const end = start + BigInt(e.value.data.byteLength);
            if (end <= current) {
              break; // Already delivered.
            }
            if (start > current) {
              complete = false;
              yield {
                kind: ExecEventKind.TRUNCATED,
                ...(isErr
                  ? { stderr: { from: current, to: start } }
                  : { stdout: { from: current, to: start } }),
              };
            }
            const data =
              start < current
                ? e.value.data.subarray(Number(current - start))
                : e.value.data;
            const offset = start < current ? current : start;
            if (isErr) {
              stderrOffset = end;
            } else {
              stdoutOffset = end;
            }
            // Delivered data ends the recovery window.
            window = undefined;
            yield {
              kind: isErr ? ExecEventKind.STDERR : ExecEventKind.STDOUT,
              offset,
              data,
            };
            break;
          }
          case "exited":
            return result({
              kind: ExecResultKind.EXITED,
              exitCode:
                e.value.result.case === "exitCode"
                  ? e.value.result.value
                  : undefined,
              signal:
                e.value.result.case === "signal"
                  ? e.value.result.value
                  : undefined,
              timedOut: e.value.timedOut,
            });
          case "failed":
            return result({
              kind: ExecResultKind.FAILED,
              reason: e.value.reason,
            });
          case "sandboxEnded":
            return result({
              kind: ExecResultKind.SANDBOX_ENDED,
              reason: e.value.reason,
            });
          case "paused":
            paused = true;
            break;
          default:
            break;
        }
      }
      if (paused) {
        // A paused sandbox ends the window; wait until it resumes or ends.
        window = undefined;
        const waited = await waitForRunning(ctx, signal);
        if (waited.result === SandboxWaitResult.ENDED) {
          return result({
            kind: ExecResultKind.SANDBOX_ENDED,
            reason: waited.reason,
          });
        }
        next = NextRequest.ATTACH;
        continue;
      }
      if (accepted) {
        // A clean stream rotation: continue at once; it ends any recovery window.
        window = undefined;
        next = NextRequest.ATTACH;
        continue;
      }
      lastError = new DataPathError(
        "relay stream ended before the agent accepted it",
        Code.Unavailable,
      );
    } catch (err) {
      if (signal.aborted) {
        throw err;
      }
      const e = ConnectError.from(err);
      if (
        sending === NextRequest.ATTACH &&
        e.code === Code.NotFound &&
        !isInterruption(err)
      ) {
        if (!started && request) {
          // Before `started`, the exec may never have reached the agent. Send
          // the identical ExecRequest; the exec_id makes this safe.
          const waited = await waitForRunning(ctx, signal, {
            onlyIfPaused: true,
          });
          if (waited.result === SandboxWaitResult.ENDED) {
            return result({
              kind: ExecResultKind.SANDBOX_ENDED,
              reason: waited.reason,
            });
          }
          if (waited.result === SandboxWaitResult.WAITED) {
            window = undefined;
          }
          if (
            ctx.clock.wallNow() - uuidV7Time(execId) >=
            OPERATION_ID_MAX_AGE_MS
          ) {
            throw new DataPathError(
              "the exec expired before the agent accepted it",
              Code.DeadlineExceeded,
              {
                cause: e,
              },
            );
          }
          next = NextRequest.EXEC;
          continue;
        }
        throw new DataPathError(
          "the agent no longer keeps this exec; its outcome is unknown",
          Code.NotFound,
          { outcomeUnknown: true, cause: e },
        );
      }
      if (!isRetryableRelayFailure(err)) {
        throw err;
      }
      lastError = err;
    }

    // A failure: start or continue the recovery window.
    if (!window) {
      window = new RecoveryWindow(ctx.clock);
    } else if (accepted) {
      window.resume();
    }
    ctx.log({
      event: LogEvent.RELAY_RECOVERY,
      rpc: sending === NextRequest.EXEC ? "Exec" : "AttachExec",
      sandboxId: ctx.sandboxId,
    });
    const delay = backoffMs(window.attempts++);
    if (window.remaining() <= delay) {
      throw lastError;
    }
    grant.discard();
    // Never run the exec again: continue with AttachExec from the received
    // offsets (from zero before `started`).
    next = NextRequest.ATTACH;
    await ctx.clock.sleep(delay, signal);
  }
}

export { SandboxEndReason };
