// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { classifyFailure, FailureClass, retryAfterMs } from "../errors.ts";
import {
  SandboxEndReason,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { LogEvent, safeLog } from "../logger.ts";
import type { OperationContext } from "../relay/context.ts";
import { Timing } from "../relay/context.ts";
import type { RecoveryWindow } from "../relay/recovery.ts";

/** Reports whether the error is a Connect `not_found`. */
export function isNotFound(error: unknown): boolean {
  return error instanceof ConnectError && error.code === Code.NotFound;
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
