// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { classifyFailure, FailureClass, retryAfterMs } from "../errors.ts";
import {
  SandboxEndReason,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { LogEvent, safeLog } from "../logger.ts";
import type { OperationContext } from "../relay/context.ts";
import { Timing, throwIfAborted } from "../relay/context.ts";
import type { RecoveryWindow } from "../relay/recovery.ts";
import { linkedController } from "../relay/stream.ts";

/** Reports whether the error is a Connect `not_found`. */
export function isNotFound(error: unknown): error is ConnectError {
  return error instanceof ConnectError && error.code === Code.NotFound;
}

/**
 * Reports whether the error is the `not_found` that the guest helper or the
 * agent returns for a `read_id` that expired ("read expired", or "operation
 * expired, or the caller's clock may be behind" for the operation ID check).
 * The `ErrorDetail` has no structured expiry reason, so the message prefix is
 * the only way to tell an expired read from a missing file. A message that
 * the peers reword makes this return false, and the read then reports the
 * peer's own message as a missing-file error, which stays a `NotFound`
 * `DataPathError`. Replace the prefix match when the protocol adds a
 * structured expiry reason.
 */
export function isExpiredRead(error: unknown): error is ConnectError {
  return (
    isNotFound(error) &&
    ["read expired", "operation expired"].some((prefix) =>
      error.rawMessage.startsWith(prefix),
    )
  );
}

/**
 * Applies the relay recovery rules to a failure: a terminal failure is
 * thrown; a retryable one starts or resumes the recovery window and waits
 * the back-off. The last error is thrown when the window has no time left.
 */
export async function recoverOrThrow(
  ctx: OperationContext,
  window: RecoveryWindow,
  failure: unknown,
  signal: AbortSignal | undefined,
): Promise<void> {
  const failureClass = classifyFailure(failure);
  if (failureClass === FailureClass.Terminal) {
    throw failure;
  }
  window.fail();
  const delay = window.nextBackoffMs(
    failureClass === FailureClass.ResourceExhausted ? retryAfterMs(failure) : 0,
  );
  if (window.remainingMs() <= delay) {
    throw failure;
  }
  safeLog(ctx.logger, {
    event: LogEvent.RelayRecovery,
    sandboxId: ctx.sandboxId,
    reason: failureClass,
    code: failure instanceof ConnectError ? Code[failure.code] : undefined,
  });
  await ctx.clock.sleep(delay, signal);
}

/** The result of waiting for a paused sandbox. */
export type SandboxWaitResult =
  { resumed: true } | { resumed: false; reason: SandboxEndReason };

/** Reads the sandbox state once. */
export async function sandboxState(
  ctx: OperationContext,
  signal: AbortSignal | undefined,
): Promise<{ state: SandboxState; endReason: SandboxEndReason }> {
  const { sandbox } = await ctx.sandboxes.getSandbox(
    { organizationId: ctx.organizationId, sandboxId: ctx.sandboxId },
    { signal },
  );
  return {
    state: sandbox?.state ?? SandboxState.UNSPECIFIED,
    endReason: sandbox?.endReason ?? SandboxEndReason.UNSPECIFIED,
  };
}

/**
 * The abort reason of a state lookup that the recovery window cut off. It is
 * private to this module's callers: `sandboxStateWithin` throws it, and the
 * caller replaces it with the last relay failure of the window.
 */
export class RecoveryDeadlineError extends Error {
  override name = "RecoveryDeadlineError";
}

/**
 * Reads the sandbox state once, bounded by the relay recovery window. While
 * the window is active, a lookup that is still pending when the window's
 * remaining time has passed is aborted and throws `RecoveryDeadlineError`.
 * The lookup does not run when the window has no time left. A caller abort
 * cancels the lookup and rejects with the caller's own reason. An inactive
 * window does not bound the lookup.
 */
export async function sandboxStateWithin(
  ctx: OperationContext,
  signal: AbortSignal | undefined,
  window: RecoveryWindow,
): Promise<{ state: SandboxState; endReason: SandboxEndReason }> {
  const remaining = window.active
    ? window.remainingMs()
    : Number.POSITIVE_INFINITY;
  if (!Number.isFinite(remaining)) {
    return sandboxState(ctx, signal);
  }
  if (remaining <= 0) {
    throwIfAborted(signal);
    throw new RecoveryDeadlineError("the recovery window has no time left");
  }
  const { controller, dispose } = linkedController(signal);
  const stop = new AbortController();
  const deadline = new RecoveryDeadlineError(
    "the sandbox state lookup outlasted the recovery window",
  );
  ctx.clock.sleep(remaining, stop.signal).then(
    () => controller.abort(deadline),
    () => {},
  );
  try {
    return await sandboxState(ctx, controller.signal);
  } catch (error) {
    // The caller's abort wins over the deadline.
    throwIfAborted(signal);
    if (controller.signal.reason === deadline) {
      throw deadline;
    }
    throw error;
  } finally {
    stop.abort();
    dispose();
  }
}

/**
 * Watches a paused sandbox until it runs again or ends, or the caller
 * cancels. It keeps waiting while the sandbox is `UNREACHABLE` and while
 * `GetSandbox` fails with a retryable error.
 */
export async function waitForSandboxToResume(
  ctx: OperationContext,
  signal: AbortSignal | undefined,
): Promise<SandboxWaitResult> {
  let delay: number = Timing.pausedPollInitialMs;
  for (;;) {
    await ctx.clock.sleep(delay, signal);
    delay = Math.min(delay * 2, Timing.pausedPollMaxMs);
    let current;
    try {
      current = await sandboxState(ctx, signal);
    } catch (error) {
      if (signal?.aborted || classifyFailure(error) === FailureClass.Terminal) {
        throw error;
      }
      continue;
    }
    switch (current.state) {
      case SandboxState.RUNNING:
        return { resumed: true };
      case SandboxState.DELETED:
      case SandboxState.FAILED:
        return { resumed: false, reason: current.endReason };
      default:
        continue;
    }
  }
}
