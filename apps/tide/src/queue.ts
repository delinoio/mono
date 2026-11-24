// SPDX-License-Identifier: Apache-2.0
import { Priority, RequestState } from "@delino/tide-protos/v1/common_pb";
import type { Request, Runner } from "@delino/tide-protos/v1/work_pb";
import { type WatchQueueEvent, WatchQueueEventKind } from "tide-sdk";
import { TERMINAL_REQUEST_STATES, TERMINAL_RUNNER_STATES } from "./format.ts";

/** The connection state of the live queue view. */
export enum LiveState {
  /** The first snapshot is loading. */
  Connecting = "connecting",
  /** The view shows the current state and receives changes. */
  Live = "live",
  /**
   * The stream ended and a new one loads its snapshot. The view keeps the
   * last state until the new snapshot is complete.
   */
  Reconnecting = "reconnecting",
  /** The stream stopped with an error. The view keeps the last state. */
  Disconnected = "disconnected",
}

/** The live queue view: non-terminal requests and runners by ID. */
export interface QueueView {
  live: LiveState;
  requests: ReadonlyMap<string, Request>;
  runners: ReadonlyMap<string, Runner>;
  /** The state that the current stream builds until its snapshot completes. */
  pending:
    | { requests: Map<string, Request>; runners: Map<string, Runner> }
    | undefined;
  /** Number of reconnects since the view opened. */
  reconnects: number;
}

/** Kinds of the view actions that do not come from `watchQueue`. */
export enum QueueViewActionKind {
  /**
   * The console opens a new `watchQueue` iterator, for example after a
   * manual retry. The new stream starts with a snapshot and without a `Gap`.
   */
  Restart = "restart",
  StreamFailed = "stream_failed",
}

/** An action of the queue view reducer. */
export type QueueViewAction =
  | WatchQueueEvent
  | { kind: QueueViewActionKind.Restart | QueueViewActionKind.StreamFailed };

/** A new view before the first snapshot. */
export function initialQueueView(): QueueView {
  return {
    live: LiveState.Connecting,
    requests: new Map(),
    runners: new Map(),
    pending: { requests: new Map(), runners: new Map() },
    reconnects: 0,
  };
}

function upsert<T>(
  map: ReadonlyMap<string, T>,
  id: string,
  value: T,
  terminal: boolean,
): Map<string, T> {
  const next = new Map(map);
  if (terminal) next.delete(id);
  else next.set(id, value);
  return next;
}

/**
 * Applies one `watchQueue` event or view action. A gap or a restart starts a
 * new snapshot, because the changes between two streams are lost; the view
 * keeps showing the last state until the new snapshot is complete and then
 * replaces it. Only a gap counts as a reconnect. Terminal
 * requests and runners leave the view. Snapshot items go into the pending
 * maps in place (set and delete are idempotent, so a repeated reducer call
 * in React strict mode is harmless) and render once the snapshot is
 * complete.
 */
export function applyQueueEvent(
  view: QueueView,
  event: QueueViewAction,
): QueueView {
  switch (event.kind) {
    case QueueViewActionKind.StreamFailed:
      return view.live === LiveState.Connecting
        ? view
        : { ...view, live: LiveState.Disconnected, pending: undefined };
    case QueueViewActionKind.Restart:
      return {
        ...view,
        live:
          view.live === LiveState.Connecting
            ? LiveState.Connecting
            : LiveState.Reconnecting,
        pending: { requests: new Map(), runners: new Map() },
      };
    case WatchQueueEventKind.Gap:
      return {
        ...view,
        live:
          view.live === LiveState.Connecting
            ? LiveState.Connecting
            : LiveState.Reconnecting,
        pending: { requests: new Map(), runners: new Map() },
        reconnects: view.reconnects + 1,
      };
    case WatchQueueEventKind.SnapshotComplete:
      if (view.pending === undefined) return { ...view, live: LiveState.Live };
      return {
        ...view,
        live: LiveState.Live,
        requests: view.pending.requests,
        runners: view.pending.runners,
        pending: undefined,
      };
    case WatchQueueEventKind.Request: {
      const r = event.request;
      const terminal = TERMINAL_REQUEST_STATES.has(r.state);
      if (view.pending !== undefined) {
        if (terminal) view.pending.requests.delete(r.requestId);
        else view.pending.requests.set(r.requestId, r);
        return view;
      }
      return {
        ...view,
        requests: upsert(view.requests, r.requestId, r, terminal),
      };
    }
    case WatchQueueEventKind.Runner: {
      const r = event.runner;
      const terminal = TERMINAL_RUNNER_STATES.has(r.state);
      if (view.pending !== undefined) {
        if (terminal) view.pending.runners.delete(r.runnerId);
        else view.pending.runners.set(r.runnerId, r);
        return view;
      }
      return {
        ...view,
        runners: upsert(view.runners, r.runnerId, r, terminal),
      };
    }
  }
}

const PRIORITY_ORDER: Record<number, number> = {
  [Priority.HIGH]: 0,
  [Priority.NORMAL]: 1,
  [Priority.LOW]: 2,
  [Priority.UNSPECIFIED]: 3,
};

/**
 * The waiting requests in queue order: priority (`HIGH` first), then the
 * time the request was queued (oldest first), then the request ID.
 */
export function waitingInQueueOrder(requests: Iterable<Request>): Request[] {
  return [...requests]
    .filter((r) => r.state === RequestState.QUEUED)
    .sort(
      (a, b) =>
        (PRIORITY_ORDER[a.priority] ?? 3) - (PRIORITY_ORDER[b.priority] ?? 3) ||
        Number((a.queuedAt?.seconds ?? 0n) - (b.queuedAt?.seconds ?? 0n)) ||
        (a.queuedAt?.nanos ?? 0) - (b.queuedAt?.nanos ?? 0) ||
        (a.requestId < b.requestId ? -1 : a.requestId > b.requestId ? 1 : 0),
    );
}
