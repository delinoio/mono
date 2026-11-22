// SPDX-License-Identifier: Apache-2.0
import type { CallOptions } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type {
  Request,
  Runner,
  WatchQueueRequestSchema,
  WatchQueueResponse,
} from "@delino/tide-protos/v1/work_pb";
import { abortedBy, backoffMs } from "./backoff.ts";
import { type Clock, systemClock } from "./clock.ts";
import { isRetryableStreamError } from "./errors.ts";

/** Kinds of the events of `watchQueue`. */
export enum WatchQueueEventKind {
  /** A request was in the snapshot, or was created or changed. */
  Request = "request",
  /** A runner was in the snapshot, or was created or changed. */
  Runner = "runner",
  /**
   * The stream sent every non-terminal request and runner. The items since
   * the last `Gap` (or since the start) are the complete current state.
   */
  SnapshotComplete = "snapshot_complete",
  /**
   * The stream ended and the helper opened a new one. Changes between the
   * two streams are lost: the new stream sends a new snapshot, so the
   * caller drops the state that it built and rebuilds it from the events
   * up to the next `SnapshotComplete`.
   */
  Gap = "gap",
}

export type WatchQueueEvent =
  | { kind: WatchQueueEventKind.Request; request: Request }
  | { kind: WatchQueueEventKind.Runner; runner: Runner }
  | { kind: WatchQueueEventKind.SnapshotComplete }
  | {
      kind: WatchQueueEventKind.Gap;
      /**
       * Consecutive streams that ended before their snapshot was complete,
       * including the last one; 0 after a stream that delivered it.
       */
      failures: number;
      /** The wait before the reconnect, in milliseconds. */
      delayMs: number;
      /** The error that ended the previous stream; undefined for a clean end. */
      cause: unknown;
    };

/** The part of the `WorkService` client that `watchQueue` uses. */
export interface WatchQueueCaller {
  watchQueue(
    request: MessageInitShape<typeof WatchQueueRequestSchema>,
    options?: CallOptions,
  ): AsyncIterable<WatchQueueResponse>;
}

export interface WatchQueueOptions {
  organizationId: string;
  /** Ends the iteration without an error. */
  signal?: AbortSignal;
  /** Time source. Default: the system clock. */
  clock?: Clock;
}

/**
 * Streams the queue of an organization and reconnects after each stream end.
 * The server ends every stream before 14 minutes, so a live view needs this.
 *
 * - After a clean stream end or a retryable failure, it waits with
 *   exponential backoff (1 second, doubled after each attempt that ended
 *   before its snapshot was complete, at most 30 seconds), yields a `Gap`
 *   event and opens a new stream. A stream that delivered its snapshot
 *   resets the backoff.
 * - Keepalive messages are not yielded.
 * - A status that is the server's answer (for example `permission_denied`
 *   or `not_found`) ends the iteration with that `ConnectError`.
 * - The signal ends the iteration without an error.
 */
export async function* watchQueue(
  work: WatchQueueCaller,
  options: WatchQueueOptions,
): AsyncGenerator<WatchQueueEvent, void, undefined> {
  const clock = options.clock ?? systemClock;
  const signal = options.signal;
  let failures = 0;
  for (;;) {
    let cause: unknown;
    let snapshotComplete = false;
    try {
      const stream = work.watchQueue(
        { organizationId: options.organizationId },
        { signal },
      );
      for await (const message of stream) {
        const event = message.event;
        switch (event.case) {
          case "request":
            yield { kind: WatchQueueEventKind.Request, request: event.value };
            break;
          case "runner":
            yield { kind: WatchQueueEventKind.Runner, runner: event.value };
            break;
          case "snapshotComplete":
            snapshotComplete = true;
            failures = 0;
            yield { kind: WatchQueueEventKind.SnapshotComplete };
            break;
          default:
            // Keepalives and events of a newer schema carry no state.
            break;
        }
      }
    } catch (error) {
      if (abortedBy(signal)) {
        return;
      }
      if (!isRetryableStreamError(error)) {
        throw error;
      }
      cause = error;
    }
    if (abortedBy(signal)) {
      return;
    }
    if (!snapshotComplete) {
      failures++;
    }
    const delayMs = backoffMs(failures - 1);
    try {
      await clock.sleep(delayMs, signal);
    } catch (error) {
      if (abortedBy(signal)) {
        return;
      }
      throw error;
    }
    yield {
      kind: WatchQueueEventKind.Gap,
      failures,
      delayMs,
      cause,
    };
  }
}
