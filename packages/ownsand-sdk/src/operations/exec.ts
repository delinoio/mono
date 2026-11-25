// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import type { DataPathSelector } from "../datapath.ts";
import { execBinding, requestDigest } from "../digest.ts";
import { DataPathError, OwnSandError } from "../errors.ts";
import {
  DataPathTransport,
  RelayOperationKind,
  SandboxEndReason,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import {
  type AttachExecRequest,
  AttachExecRequestSchema,
  type ExecEvent,
  ExecFailureReason,
  type ExecRequest,
} from "@delino/ownsand-protos/v1/data_frame_pb";
import { PeerUnavailableError, SessionEndedError } from "../p2p/session.ts";
import {
  type OperationContext,
  Timing,
  throwIfAborted,
} from "../relay/context.ts";
import { GrantKeeper, RelayCallKind } from "../relay/grant.ts";
import { RecoveryWindow } from "../relay/recovery.ts";
import { type ConsumerHold, watchedRelayStream } from "../relay/stream.ts";
import { uuidV7TimestampMs } from "../uuidv7.ts";
import {
  isFailedPrecondition,
  isNotFound,
  RecoveryDeadlineError,
  recoverOrThrow,
  sandboxStateWithin,
  waitForSandboxToResume,
} from "./common.ts";

/** Event types of an exec stream. */
export enum ExecEventType {
  Started = "started",
  Stdout = "stdout",
  Stderr = "stderr",
  /** Output was lost; the result is marked incomplete. */
  OutputTruncated = "output_truncated",
}

/** A missing byte range `[start, end)` of an output stream. */
export interface ByteRange {
  start: bigint;
  end: bigint;
}

export type ExecStreamEvent =
  | { type: ExecEventType.Started; execId: string }
  | {
      type: ExecEventType.Stdout | ExecEventType.Stderr;
      offset: bigint;
      data: Uint8Array;
    }
  | {
      type: ExecEventType.OutputTruncated;
      stdout: ByteRange | undefined;
      stderr: ByteRange | undefined;
    };

/** How an exec ended. A data path failure is an error, never a result. */
export enum ExecResultKind {
  /** The command exited or a signal ended it. */
  Exited = "exited",
  /** The command could not start. */
  Failed = "failed",
  /** The sandbox ended before the result could be delivered. */
  SandboxEnded = "sandbox_ended",
}

interface ExecResultBase {
  /** False when output was lost (`output_truncated`). */
  complete: boolean;
}

export type ExecResult = ExecResultBase &
  (
    | {
        kind: ExecResultKind.Exited;
        exitCode: bigint | undefined;
        signal: number | undefined;
        timedOut: boolean;
      }
    | {
        kind: ExecResultKind.Failed;
        reason: ExecFailureReason;
        message: string;
      }
    | { kind: ExecResultKind.SandboxEnded; reason: SandboxEndReason }
  );

/** What to run or attach to. */
export interface ExecPlan {
  execId: string;
  /** The original request; absent for an attach-only caller. */
  request: ExecRequest | undefined;
  stdoutOffset: bigint;
  stderrOffset: bigint;
}

enum Mode {
  Exec = "exec",
  Attach = "attach",
}

/**
 * Runs or continues one exec under the data path rules of
 * docs/packages-ownsand-sdk-foundation.md. It never runs a command twice:
 * after any failure it continues with `AttachExec`, and it resends the
 * identical `ExecRequest` only before `started` when `AttachExec` returns
 * `not_found`.
 */
export async function* runExec(
  ctx: OperationContext,
  selector: DataPathSelector,
  plan: ExecPlan,
  signal: AbortSignal | undefined,
  hold?: ConsumerHold,
): AsyncGenerator<ExecStreamEvent, ExecResult> {
  const request = plan.request;
  const attachOnly = request === undefined;
  const grants = new GrantKeeper(
    ctx,
    attachOnly ? RelayOperationKind.ATTACH_EXEC : RelayOperationKind.EXEC,
    plan.execId,
    attachOnly ? undefined : requestDigest(execBinding(request)),
  );
  const window = new RecoveryWindow(ctx.clock);
  let mode = attachOnly ? Mode.Attach : Mode.Exec;
  let started = false;
  let complete = true;
  let stdoutNext = plan.stdoutOffset;
  let stderrNext = plan.stderrOffset;
  // The last failure that fed the recovery window. A state lookup that the
  // window cuts off ends the operation with it.
  let lastRelayFailure: unknown;
  // Whether the `ExecRequest` of this iteration is a resend after AttachExec
  // answered not_found, and not the first send of the request.
  let resending = false;

  /**
   * The sandbox is paused and the exec did not start: the pause may outlast
   * the recovery window, so the window ends and the SDK waits for the
   * sandbox to resume. Returns the result when the sandbox ended instead; it
   * returns undefined when the identical request can be resent.
   */
  const resumeFromPause = async (
    cause: unknown,
  ): Promise<ExecResult | undefined> => {
    window.end();
    const waited = await waitForSandboxToResume(ctx, signal);
    if (!waited.resumed) {
      return {
        kind: ExecResultKind.SandboxEnded,
        reason: waited.reason,
        complete,
      };
    }
    if (
      ctx.clock.wallNow() - uuidV7TimestampMs(plan.execId) >=
      Timing.operationIdLifetimeMs
    ) {
      throw new DataPathError(
        "the exec did not start before its exec_id expired",
        { code: Code.DeadlineExceeded, cause },
      );
    }
    return undefined;
  };

  const attachRequest = (): AttachExecRequest =>
    create(AttachExecRequestSchema, {
      organizationId: ctx.organizationId,
      sandboxId: ctx.sandboxId,
      execId: plan.execId,
      stdoutOffset: stdoutNext,
      stderrOffset: stderrNext,
    });

  for (;;) {
    throwIfAborted(signal);
    const session = await selector.select(signal);
    const onRelay = session === undefined;
    let events: AsyncIterable<ExecEvent | undefined>;
    if (onRelay) {
      let grantId: string;
      try {
        grantId = await grants.acquire(
          RelayCallKind.ServerStream,
          signal,
          window.active ? window : undefined,
        );
      } catch (error) {
        throwIfAborted(signal);
        throw error;
      }
      grants.streamStarted();
      const sendExec = mode === Mode.Exec;
      events = mapEvents(
        watchedRelayStream<{ event?: ExecEvent | undefined }>(
          ctx,
          grantId,
          signal,
          (options) =>
            sendExec
              ? ctx.relay.exec(request!, options)
              : ctx.relay.attachExec(attachRequest(), options),
          {
            window,
            isAccepted: (response) =>
              response.event?.event?.case === "accepted",
          },
          hold,
          (response) => {
            const kind = response.event?.event?.case;
            return (
              kind === "exited" || kind === "failed" || kind === "sandboxEnded"
            );
          },
        ),
      );
    } else {
      events = session.exec(
        mode === Mode.Exec
          ? { case: "execRequest", value: request! }
          : { case: "execAttachRequest", value: attachRequest() },
        signal,
      );
    }

    // P2P streams have no `accepted` event and do not rotate.
    let accepted = false;
    let paused = false;
    let failure: unknown;
    try {
      for await (const event of events) {
        const e = event?.event;
        switch (e?.case) {
          case "accepted":
            accepted = true;
            window.pause();
            break;
          case "started":
            if (!started) {
              started = true;
              yield { type: ExecEventType.Started, execId: plan.execId };
            }
            break;
          case "outputTruncated": {
            const stdout =
              e.value.stdoutOffset > stdoutNext
                ? { start: stdoutNext, end: e.value.stdoutOffset }
                : undefined;
            const stderr =
              e.value.stderrOffset > stderrNext
                ? { start: stderrNext, end: e.value.stderrOffset }
                : undefined;
            if (stdout !== undefined || stderr !== undefined) {
              complete = false;
              if (stdout) stdoutNext = stdout.end;
              if (stderr) stderrNext = stderr.end;
              yield { type: ExecEventType.OutputTruncated, stdout, stderr };
            }
            break;
          }
          case "stdout":
          case "stderr": {
            const isStdout = e.case === "stdout";
            const next = isStdout ? stdoutNext : stderrNext;
            const { offset, data } = e.value;
            if (offset > next) {
              // Output between the delivered offset and this chunk is gone;
              // report it instead of presenting the output as complete.
              complete = false;
              yield {
                type: ExecEventType.OutputTruncated,
                stdout: isStdout ? { start: next, end: offset } : undefined,
                stderr: isStdout ? undefined : { start: next, end: offset },
              };
              throwIfAborted(signal);
            }
            const end = offset + BigInt(data.length);
            const from = offset > next ? offset : next;
            if (end <= from) {
              break;
            }
            const fresh = data.subarray(Number(from - offset));
            if (isStdout) stdoutNext = end;
            else stderrNext = end;
            window.end();
            yield {
              type: isStdout ? ExecEventType.Stdout : ExecEventType.Stderr,
              offset: from,
              data: fresh,
            };
            break;
          }
          case "exited":
            window.end();
            return {
              kind: ExecResultKind.Exited,
              exitCode:
                e.value.status.case === "exitCode"
                  ? e.value.status.value
                  : undefined,
              signal:
                e.value.status.case === "signal"
                  ? e.value.status.value
                  : undefined,
              timedOut: e.value.timedOut,
              complete,
            };
          case "failed":
            window.end();
            return {
              kind: ExecResultKind.Failed,
              reason: e.value.reason,
              message: e.value.message,
              complete,
            };
          case "sandboxEnded":
            window.end();
            return {
              kind: ExecResultKind.SandboxEnded,
              reason: e.value.reason,
              complete,
            };
          case "paused":
            paused = true;
            break;
          default:
            // `keepalive` events and cases of a newer schema.
            break;
        }
        if (paused) {
          break;
        }
      }
      if (paused) {
        // The exec has not ended and the sandbox is paused: wait for it to
        // resume or end.
        window.end();
        if (onRelay) grants.streamEnded();
        const waited = await waitForSandboxToResume(ctx, signal);
        if (!waited.resumed) {
          return {
            kind: ExecResultKind.SandboxEnded,
            reason: waited.reason,
            complete,
          };
        }
        mode = Mode.Attach;
        continue;
      }
      if (onRelay) grants.streamEnded();
      if (onRelay && accepted) {
        // A routine rotation at the relay stream limit.
        window.end();
        mode = Mode.Attach;
        continue;
      }
      failure = new UnacceptedStreamEnd(
        "relay stream ended before the agent accepted it",
      );
    } catch (error) {
      if (onRelay) grants.streamEnded();
      failure = error;
    }

    throwIfAborted(signal);
    if (failure instanceof SessionEndedError) {
      if (session !== undefined) selector.sessionEnded(session);
      mode = Mode.Attach;
      continue;
    }
    if (failure instanceof PeerUnavailableError) {
      selector.peerFailed(failure, session);
      lastRelayFailure = failure;
      window.fail();
      mode = Mode.Attach;
      continue;
    }
    if (isNotFound(failure) && mode === Mode.Attach) {
      if (started || attachOnly) {
        throw new DataPathError(
          "the agent no longer knows the exec; its outcome is unknown",
          { code: Code.NotFound, outcomeUnknown: true, cause: failure },
        );
      }
      // Before `started`, the command did not start yet: send the identical
      // request (the same exec_id makes this safe).
      let current;
      try {
        current = await sandboxStateWithin(ctx, signal, window);
      } catch (error) {
        throwIfAborted(signal);
        if (error instanceof RecoveryDeadlineError) {
          // The lookup outlasted the recovery window: end with the window's
          // last relay failure and send no further request.
          throw lastRelayFailure ?? failure;
        }
        current = undefined;
      }
      if (current?.state === SandboxState.PAUSED) {
        const ended = await resumeFromPause(failure);
        if (ended !== undefined) return ended;
      } else if (
        current?.state === SandboxState.DELETED ||
        current?.state === SandboxState.FAILED
      ) {
        return {
          kind: ExecResultKind.SandboxEnded,
          reason: current.endReason,
          complete,
        };
      }
      resending = true;
      mode = Mode.Exec;
      continue;
    }
    if (
      isFailedPrecondition(failure) &&
      mode === Mode.Exec &&
      resending &&
      !started &&
      request !== undefined
    ) {
      // The agent rejected the resend. A sandbox that paused meanwhile
      // rejects it with failed_precondition: wait for the resume as for a
      // paused sandbox that has no exec record, then resend the identical
      // request. Any other state, or a failed lookup, keeps the original
      // error.
      let current;
      try {
        current = await sandboxStateWithin(ctx, signal, window);
      } catch {
        throwIfAborted(signal);
        current = undefined;
      }
      if (current?.state === SandboxState.PAUSED) {
        const ended = await resumeFromPause(failure);
        if (ended !== undefined) return ended;
        mode = Mode.Exec;
        continue;
      }
      if (
        current?.state === SandboxState.DELETED ||
        current?.state === SandboxState.FAILED
      ) {
        return {
          kind: ExecResultKind.SandboxEnded,
          reason: current.endReason,
          complete,
        };
      }
    }
    if (onRelay) grants.discardFor(failure);
    lastRelayFailure = failure;
    await recoverOrThrow(ctx, window, failure, signal);
    mode = Mode.Attach;
  }
}

/** A relay stream that ended with OK before `accepted`: not a rotation. */
class UnacceptedStreamEnd extends OwnSandError {
  override name = "UnacceptedStreamEnd";
}

async function* mapEvents(
  responses: AsyncIterable<{ event?: ExecEvent | undefined }>,
): AsyncGenerator<ExecEvent | undefined> {
  for await (const response of responses) {
    yield response.event;
  }
}

/**
 * The handle of one exec. Iterate it for the events; `wait()` drains the
 * remaining events and returns the result.
 */
export class ExecHandle implements AsyncIterable<ExecStreamEvent> {
  readonly execId: string;
  readonly #generator: AsyncGenerator<ExecStreamEvent, ExecResult | undefined>;
  readonly #selector: DataPathSelector;
  readonly #hold: ConsumerHold | undefined;
  #iterated = false;
  #result: ExecResult | undefined;
  /**
   * The first failure of the stream. It is boxed because an abort reason
   * can be any value, including `undefined`, so the box tells "failed" apart
   * from "not failed".
   */
  #failure: { readonly error: unknown } | undefined;
  #draining: Promise<void> | undefined;
  /**
   * The first read of the generator when the handle starts the exec at
   * creation. It settles to a box, so a handle that nobody reads never
   * produces an unhandled rejection. The first reader takes it.
   */
  #primed:
    | {
        readonly read: Promise<
          | { readonly next: IteratorResult<ExecStreamEvent, ExecResult> }
          | { readonly error: unknown }
        >;
        readonly callerSignal: AbortSignal | undefined;
      }
    | undefined;

  /**
   * With `startNow`, the generator makes its first read before the
   * constructor returns, so that the exec request goes out when the caller
   * starts the exec and not when it first reads the handle. The exec ID is
   * minted at that moment, and the agent admits an unknown ID only for 15
   * minutes after its timestamp. Pass the `hold` of the generator, created
   * unclaimed, so that a handle that is never read still lets the idle
   * limit end the stream.
   */
  constructor(
    execId: string,
    generator: AsyncGenerator<ExecStreamEvent, ExecResult>,
    selector: DataPathSelector,
    hold?: ConsumerHold,
    startNow = false,
    callerSignal?: AbortSignal,
  ) {
    this.#hold = hold;
    this.execId = execId;
    this.#generator = generator;
    this.#selector = selector;
    if (startNow) {
      this.#primed = {
        read: generator.next().then(
          (next) => ({ next }),
          (error: unknown) => ({ error }),
        ),
        callerSignal,
      };
    }
  }

  /** The transport that the sandbox handle uses now. */
  get transport(): DataPathTransport {
    return this.#selector.transport;
  }

  async *[Symbol.asyncIterator](): AsyncGenerator<ExecStreamEvent> {
    if (this.#iterated) {
      throw new OwnSandError("an exec handle can be iterated only once");
    }
    this.#iterated = true;
    try {
      for (;;) {
        const next = await this.#read();
        if (next.done) {
          // Keep a result that a concurrent wait() already stored.
          this.#result ??= next.value;
          return;
        }
        yield next.value;
      }
    } finally {
      // Leaving the loop early (`break`, `return` or a thrown error) leaves
      // the generator suspended with its stream open. Tell the stream that
      // no event is held, so the idle limit applies and an abandoned stream
      // ends. A later wait() resumes the same stream, or continues with
      // `AttachExec` after the idle limit.
      this.#hold?.release();
    }
  }

  /** Drains the remaining events and returns the result. */
  async wait(): Promise<ExecResult> {
    // Decide on whether the underlying generator finished, not on whether
    // iteration started: a consumer can break out of a for-await early, which
    // leaves the generator suspended with its stream open and no result yet.
    this.#iterated = true;
    if (this.#result === undefined) {
      // A terminal failure is final: every later wait() reports the same
      // rejection, because the closed generator has nothing more to say.
      if (this.#failure !== undefined) throw this.#failure.error;
      // Overlapping wait() calls share one drain. A second reader of the
      // generator would see its exhausted `{ done: true }` and must never be
      // able to erase the terminal result that the first reader stored.
      this.#draining ??= this.#drain().finally(() => {
        this.#draining = undefined;
      });
      await this.#draining;
    }
    if (this.#result === undefined) {
      throw new OwnSandError("the exec ended without a result");
    }
    return this.#result;
  }

  async #drain(): Promise<void> {
    while (this.#result === undefined) {
      const next = await this.#read();
      if (next.done) {
        // Keep a result that a concurrent reader already stored.
        this.#result ??= next.value;
        return;
      }
      // Discard events.
    }
  }

  /**
   * The one place that reads the generator, for iteration and for draining.
   *
   * A generator that rejects closes. A read that was queued behind the
   * rejecting read then resolves with `{ done: true }` and no result, which
   * would hide the failure from its consumer. The first rejection is recorded
   * and every read that ends without a result after it, and every later
   * read, rethrows the same rejection.
   */
  async #read(): Promise<
    IteratorResult<ExecStreamEvent, ExecResult | undefined>
  > {
    if (this.#failure !== undefined && this.#result === undefined) {
      throw this.#failure.error;
    }
    let next: IteratorResult<ExecStreamEvent, ExecResult | undefined>;
    try {
      const primed = this.#primed;
      if (primed !== undefined) {
        // The event that the stream yielded at creation is now held by this
        // reader, so the idle clock pauses until the reader asks for more.
        this.#hold?.claim();
        const settled = await primed.read;
        // Readers wait for the same first read. Only the first reader can
        // take the cached event or close the stream and record cancellation.
        if (this.#primed === primed) {
          this.#primed = undefined;
          if ("error" in settled) throw settled.error;
          if (!settled.next.done && primed.callerSignal?.aborted) {
            this.#failure = { error: primed.callerSignal.reason };
            await this.#generator.return(undefined);
            throw this.#failure.error;
          }
          return settled.next;
        }
        if (this.#failure !== undefined) throw this.#failure.error;
      }
      next = await this.#generator.next();
    } catch (error) {
      this.#failure ??= { error };
      throw this.#failure.error;
    }
    if (
      next.done &&
      this.#failure !== undefined &&
      this.#result === undefined
    ) {
      throw this.#failure.error;
    }
    return next;
  }
}

export { ExecFailureReason };
