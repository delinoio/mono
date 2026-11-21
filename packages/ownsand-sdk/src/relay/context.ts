// SPDX-License-Identifier: Apache-2.0
import type { Client } from "@connectrpc/connect";
import type { Clock } from "../clock.ts";
import type { ActivityRegistry } from "../connection.ts";
import type { DataPathService } from "../gen/delino/ownsand/v1/data_path_pb.ts";
import type { RelayService } from "../gen/delino/ownsand/v1/relay_pb.ts";
import type { SandboxService } from "../gen/delino/ownsand/v1/sandbox_pb.ts";
import type { Logger } from "../logger.ts";
import type { UuidV7Generator } from "../uuidv7.ts";

/** Everything an operation needs to reach the control plane and the relay. */
export interface OperationContext {
  readonly clock: Clock;
  readonly ids: UuidV7Generator;
  readonly dataPath: Client<typeof DataPathService>;
  readonly relay: Client<typeof RelayService>;
  readonly sandboxes: Client<typeof SandboxService>;
  readonly activity: ActivityRegistry;
  readonly logger: Logger | undefined;
  readonly organizationId: string;
  readonly sandboxId: string;
}

/** Timing rules of docs/packages-ownsand-sdk-foundation.md, in milliseconds. */
export const Timing = {
  /** Length of one relay recovery window and of each authorization retry window. */
  recoveryWindowMs: 60_000,
  /** The control plane's relay server stream limit. */
  relayStreamLimitMs: 14 * 60_000,
  /** The control plane's relay WriteFile call limit. */
  relayWriteCallLimitMs: 5 * 60_000,
  /** Margin added to a stream or call limit before a grant is used. */
  grantMarginMs: 30_000,
  /** A grant whose last stream ended longer ago is renewed. */
  grantIdleMs: 60_000,
  /** A stream that receives no byte for this long is an interruption. */
  streamIdleMs: 90_000,
  /** SDK-side deadline of one WriteFile attempt. */
  writeAttemptDeadlineMs: 5 * 60_000 + 30_000,
  /** First and largest back-off delay between recovery attempts. */
  backoffInitialMs: 500,
  backoffMaxMs: 5_000,
  /** Poll interval while the SDK waits for a paused sandbox. */
  pausedPollInitialMs: 2_000,
  pausedPollMaxMs: 10_000,
  /** An operation ID older than this is expired at the agent. */
  operationIdLifetimeMs: 15 * 60_000,
  /**
   * The data channel must open within this time after the answer is applied;
   * ICE gathering has the same limit, counted from the start of gathering.
   */
  dataChannelOpenMs: 10_000,
} as const;

/** Rejects when the signal is aborted. */
export function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted) {
    throw signal.reason;
  }
}
