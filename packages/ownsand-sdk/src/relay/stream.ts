// SPDX-License-Identifier: Apache-2.0
import { ACTIVITY_HEADER, RELAY_GRANT_HEADER } from "../connection.ts";
import { Code } from "@connectrpc/connect";
import { DataPathError, OwnSandError } from "../errors.ts";
import { type OperationContext, Timing } from "./context.ts";
import type { RecoveryWindow } from "./recovery.ts";

/**
 * A relay stream that received no byte of its response body for the idle
 * limit. The SDK cancels it and treats it as a transport interruption.
 */
export class StreamIdleError extends OwnSandError {
  override name = "StreamIdleError";
}

/**
 * A recovery continuation that the agent did not accept before the recovery
 * window ended. The SDK cancels the stream; it is a `DataPathError` with the
 * Connect code `unavailable`, like an `AuthorizeRelay` call that gets no
 * response within the window. The window has no time left, so the operation
 * ends with it.
 */
export class AcceptanceDeadlineError extends DataPathError {
  override name = "AcceptanceDeadlineError";

  constructor(message: string, options: { cause?: unknown } = {}) {
    super(message, { code: Code.Unavailable, cause: options.cause });
  }
}

/**
 * Bounds a recovery continuation by the time left in its recovery window
 * until the agent accepts the stream. Frames before `accepted` (for example a
 * relay `keepalive`) do not extend the bound.
 */
export interface AcceptanceBound<T> {
  window: RecoveryWindow;
  /** Reports whether the response is the `accepted` event. */
  isAccepted: (value: T) => boolean;
}

/** A WriteFile attempt that passed its SDK-side deadline. */
export class AttemptDeadlineError extends OwnSandError {
  override name = "AttemptDeadlineError";
}

export interface CallOptions {
  signal: AbortSignal;
  headers: Record<string, string>;
}

/**
 * Creates a controller that aborts with the caller signal's reason. The
 * caller must call `dispose` when the call ends; otherwise the abort listener
 * stays on a long-lived caller signal for every call.
 */
export function linkedController(signal: AbortSignal | undefined): {
  controller: AbortController;
  dispose: () => void;
} {
  const controller = new AbortController();
  if (!signal) {
    return { controller, dispose: () => {} };
  }
  if (signal.aborted) {
    controller.abort(signal.reason);
    return { controller, dispose: () => {} };
  }
  const onAbort = () => controller.abort(signal.reason);
  signal.addEventListener("abort", onAbort, { once: true });
  return {
    controller,
    dispose: () => signal.removeEventListener("abort", onAbort),
  };
}

/**
 * Tells a relay stream that its consumer no longer holds an event. A consumer
 * that leaves its iteration early (`break`, `return` or an exception in the
 * loop body) releases the hold, so the idle limit starts again for the
 * suspended stream instead of treating the last event as held forever. The
 * release is forward-only: a released hold never becomes active again.
 */
export class ConsumerHold {
  #released = false;
  readonly #listeners = new Set<() => void>();

  get released(): boolean {
    return this.#released;
  }

  release(): void {
    if (this.#released) {
      return;
    }
    this.#released = true;
    for (const listener of [...this.#listeners]) {
      listener();
    }
  }

  /** Registers a release listener and returns the function that removes it. */
  onRelease(listener: () => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }
}

/**
 * Runs one relay server stream with the grant header, byte activity
 * tracking and the idle limit. The idle clock runs only while the SDK waits
 * for the stream, not while the caller holds an event. While the recovery
 * window of `bound` is active, the stream also ends when the window's
 * remaining time passes before the agent accepts it. When the consumer
 * releases `hold` (it left its iteration early), the held event no longer
 * pauses the idle clock: the call is aborted after the idle limit and the
 * stream's timers and listeners are released even if nobody resumes the
 * generator. A caller abort releases them the same way while an event is
 * held.
 */
export async function* watchedRelayStream<T>(
  ctx: OperationContext,
  grantId: string,
  callerSignal: AbortSignal | undefined,
  open: (options: CallOptions) => AsyncIterable<T>,
  bound?: AcceptanceBound<T>,
  hold?: ConsumerHold,
): AsyncGenerator<T> {
  const clock = ctx.clock;
  const { controller: call, dispose: unlink } = linkedController(callerSignal);
  const stopWatch = new AbortController();
  let lastActivity = clock.now();
  let holding = false;
  let idleFired = false;
  const registration = ctx.activity.register(() => {
    lastActivity = clock.now();
  });
  // A release wakes the watcher's sleep so that it recomputes its wait.
  let wake = new AbortController();
  const offRelease = hold?.onRelease(() => {
    lastActivity = clock.now();
    wake.abort();
  });
  const watcher = (async () => {
    for (;;) {
      const held = holding && hold?.released !== true;
      const wait = held
        ? Timing.streamIdleMs
        : lastActivity + Timing.streamIdleMs - clock.now();
      if (wait <= 0) {
        idleFired = true;
        call.abort(new StreamIdleError("relay stream idle limit reached"));
        if (holding) {
          // The consumer abandoned the iteration, so the generator stays
          // suspended at its `yield` and its `finally` does not run until
          // someone resumes it. Release what the stream holds now; the
          // later `finally` repeats both calls harmlessly.
          registration.dispose();
          unlink();
        }
        return;
      }
      try {
        await clock.sleep(
          wait,
          AbortSignal.any([stopWatch.signal, call.signal, wake.signal]),
        );
      } catch {
        if (stopWatch.signal.aborted || call.signal.aborted) {
          if (holding && !stopWatch.signal.aborted) {
            // The caller cancelled the call while the consumer holds or has
            // abandoned an event, so the generator is suspended at its
            // `yield` and its `finally` does not run until someone resumes
            // it. Release the registration and the caller-signal listener
            // now; the later `finally` repeats both calls harmlessly.
            registration.dispose();
            unlink();
          }
          return;
        }
        // A release woke the sleep: arm a new wake signal and look again.
        wake = new AbortController();
      }
    }
  })().catch(() => {});
  // The acceptance bound applies only to a continuation in recovery, and ends
  // when the agent accepts it, the window ends (delivered data) or the stream
  // closes. The timer re-reads the window, so a clock that pauses or an
  // ended window never aborts an accepted or recovered stream.
  const stopDeadline = new AbortController();
  const deadline =
    bound?.window.active === true
      ? (async () => {
          for (;;) {
            const left = bound.window.remainingMs();
            if (!Number.isFinite(left)) {
              return;
            }
            if (left <= 0) {
              call.abort(
                new AcceptanceDeadlineError(
                  "relay continuation not accepted within the recovery window",
                ),
              );
              return;
            }
            await clock.sleep(left, stopDeadline.signal);
          }
        })().catch(() => {})
      : undefined;
  try {
    const iterator = open({
      signal: call.signal,
      headers: {
        [RELAY_GRANT_HEADER]: grantId,
        [ACTIVITY_HEADER]: registration.id,
      },
    })[Symbol.asyncIterator]();
    for (;;) {
      let result: IteratorResult<T>;
      try {
        result = await iterator.next();
      } catch (error) {
        if (
          !callerSignal?.aborted &&
          call.signal.reason instanceof AcceptanceDeadlineError
        ) {
          throw new AcceptanceDeadlineError(call.signal.reason.message, {
            cause: error,
          });
        }
        if (idleFired && !callerSignal?.aborted) {
          throw new StreamIdleError("relay stream idle limit reached", {
            cause: error,
          });
        }
        throw error;
      }
      if (result.done) {
        return;
      }
      lastActivity = clock.now();
      if (bound?.isAccepted(result.value)) {
        stopDeadline.abort();
      }
      holding = true;
      yield result.value;
      holding = false;
      lastActivity = clock.now();
    }
  } finally {
    stopWatch.abort();
    offRelease?.();
    stopDeadline.abort();
    registration.dispose();
    call.abort(new OwnSandError("relay stream closed"));
    await watcher;
    await deadline;
    unlink();
  }
}

/** Runs one unary relay call with the grant header and a deadline. */
export async function relayCallWithDeadline<T>(
  ctx: OperationContext,
  grantId: string,
  callerSignal: AbortSignal | undefined,
  deadlineMs: number,
  call: (options: CallOptions) => Promise<T>,
): Promise<T> {
  const { controller, dispose: unlink } = linkedController(callerSignal);
  const stopTimer = new AbortController();
  let deadlinePassed = false;
  const timer = ctx.clock
    .sleep(deadlineMs, stopTimer.signal)
    .then(() => {
      deadlinePassed = true;
      controller.abort(new AttemptDeadlineError("WriteFile attempt deadline"));
    })
    .catch(() => {});
  try {
    return await call({
      signal: controller.signal,
      headers: { [RELAY_GRANT_HEADER]: grantId },
    });
  } catch (error) {
    if (deadlinePassed && !callerSignal?.aborted) {
      throw new AttemptDeadlineError("WriteFile attempt deadline", {
        cause: error,
      });
    }
    throw error;
  } finally {
    stopTimer.abort();
    await timer;
    unlink();
  }
}
