// SPDX-License-Identifier: Apache-2.0
import { ConnectError } from "@connectrpc/connect";

import {
  SandboxEndReason,
  SandboxState,
} from "../gen/delino/ownsand/v1/common_pb.ts";
import type { RelayContext } from "./context.ts";
import { backoffMs, isRetryableRelayFailure } from "./recovery.ts";

/** Interval between sandbox state checks while the SDK waits for a resume. */
export const PAUSED_POLL_MS = 5_000;

export enum SandboxWaitResult {
  /** The sandbox was running; the SDK did not wait. */
  RUNNING = "running",
  /** The SDK waited until the sandbox was running again. */
  WAITED = "waited",
  /** The sandbox ended. */
  ENDED = "ended",
}

export type SandboxWait =
  | { result: SandboxWaitResult.RUNNING | SandboxWaitResult.WAITED }
  | { result: SandboxWaitResult.ENDED; reason: SandboxEndReason };

/**
 * Watches the sandbox with `GetSandbox` until it runs, ends, or the caller
 * cancels. With `onlyIfPaused`, it returns at once unless the sandbox is
 * `PAUSED`.
 */
export async function waitForRunning(
  ctx: RelayContext,
  signal: AbortSignal,
  options: { onlyIfPaused?: boolean } = {},
): Promise<SandboxWait> {
  let waited = false;
  let failures = 0;
  for (;;) {
    signal.throwIfAborted();
    let state: SandboxState;
    let reason = SandboxEndReason.UNSPECIFIED;
    try {
      const sandbox = await ctx.getSandbox(signal);
      state = sandbox.state;
      reason = sandbox.endReason;
      failures = 0;
    } catch (err) {
      if (signal.aborted || !isRetryableRelayFailure(err)) {
        throw ConnectError.from(err);
      }
      await ctx.clock.sleep(backoffMs(failures++), signal);
      continue;
    }
    switch (state) {
      case SandboxState.RUNNING:
        return {
          result: waited ? SandboxWaitResult.WAITED : SandboxWaitResult.RUNNING,
        };
      case SandboxState.DELETED:
      case SandboxState.FAILED:
        return { result: SandboxWaitResult.ENDED, reason };
      case SandboxState.PAUSED:
        break;
      default:
        if (options.onlyIfPaused && !waited) {
          return { result: SandboxWaitResult.RUNNING };
        }
        break;
    }
    waited = true;
    await ctx.clock.sleep(PAUSED_POLL_MS, signal);
  }
}
