// SPDX-License-Identifier: Apache-2.0
import { ACTIVITY_HEADER, RELAY_GRANT_HEADER } from "../connection.ts";
import { OwnSandError } from "../errors.ts";
import { type OperationContext, Timing } from "./context.ts";

/**
 * A relay stream that received no byte of its response body for the idle
 * limit. The SDK cancels it and treats it as a transport interruption.
 */
export class StreamIdleError extends OwnSandError {
  override name = "StreamIdleError";
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
 * Runs one relay server stream with the grant header, byte activity
 * tracking and the idle limit. The idle clock runs only while the SDK waits
 * for the stream, not while the caller holds an event.
 */
export async function* watchedRelayStream<T>(
  ctx: OperationContext,
  grantId: string,
  callerSignal: AbortSignal | undefined,
  open: (options: CallOptions) => AsyncIterable<T>,
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
  const watcher = (async () => {
    for (;;) {
      const wait = holding
        ? Timing.streamIdleMs
        : lastActivity + Timing.streamIdleMs - clock.now();
      if (wait <= 0) {
        idleFired = true;
        call.abort(new StreamIdleError("relay stream idle limit reached"));
        return;
      }
      await clock.sleep(wait, stopWatch.signal);
    }
  })().catch(() => {});
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
      holding = true;
      yield result.value;
      holding = false;
      lastActivity = clock.now();
    }
  } finally {
    stopWatch.abort();
    registration.dispose();
    call.abort(new OwnSandError("relay stream closed"));
    await watcher;
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
