// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import {
  createClient,
  createConnectRouter,
  createHandlerContext,
  createRouterTransport,
  type ServiceImpl,
} from "@connectrpc/connect";
import * as actualConnect from "@connectrpc/connect" with {
  rstest: "importActual",
};
import {
  AuditAction,
  AuditService,
  AuditTargetKind,
} from "@delino/sandrun-protos/v1/audit_pb";
import {
  RequestEndReason,
  RequestState,
  RunnerEndReason,
  RunnerState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import {
  CancelRequestRequestSchema,
  GetRunnerRequestSchema,
  WatchQueueRequestSchema,
  WorkService,
} from "@delino/sandrun-protos/v1/work_pb";
import { afterEach, beforeEach, expect, rs, test } from "@rstest/core";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
  type MockTransportOptions,
} from "./mock-api.ts";

rs.mock(import("@connectrpc/connect"), () => ({
  ...actualConnect,
  createRouterTransport: rs.fn(actualConnect.createRouterTransport),
}));

beforeEach(() => {
  rs.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
});

afterEach(() => {
  rs.useRealTimers();
});

function openWatch(organizationId: string, options?: MockTransportOptions) {
  const transport = createMockTransport(options);
  const register = rs.mocked(createRouterTransport).mock.lastCall?.[0];
  if (!register) throw new Error("Expected mock route registration");
  const router = createConnectRouter();
  const service = rs.spyOn(router, "service");
  register(router);
  const work = service.mock.calls.find(
    ([desc]) => desc === WorkService,
  )?.[1] as ServiceImpl<typeof WorkService> | undefined;
  if (!work) throw new Error("Expected the mock work service");

  // Read the handler directly so transport shutdown cannot mask a leak.
  const context = createHandlerContext({
    service: WorkService,
    method: WorkService.method.watchQueue,
    protocolName: "connect",
    requestMethod: "POST",
    url: "https://in-memory",
  });
  const add = rs.spyOn(context.signal, "addEventListener");
  const remove = rs.spyOn(context.signal, "removeEventListener");
  return {
    context,
    transport,
    work: createClient(WorkService, transport),
    audit: createClient(AuditService, transport),
    getStoredRunner: async (runnerId: string) => {
      const { runner } = await work.getRunner(
        create(GetRunnerRequestSchema, { organizationId, runnerId }),
        context,
      );
      if (!runner) throw new Error("Expected a stored runner");
      return runner;
    },
    stream: work
      .watchQueue(create(WatchQueueRequestSchema, { organizationId }), context)
      [Symbol.asyncIterator](),
    cancelRequest: (requestId: string) =>
      work.cancelRequest(
        create(CancelRequestRequestSchema, { organizationId, requestId }),
        context,
      ),
    pendingAbortListeners: () =>
      add.mock.calls.filter(
        ([type, listener]) =>
          type === "abort" &&
          !remove.mock.calls.some(
            ([removedType, removedListener]) =>
              removedType === type && removedListener === listener,
          ),
      ).length,
  };
}

async function readSnapshot(stream: ReturnType<typeof openWatch>["stream"]) {
  const events = [];
  for (;;) {
    const { value, done } = await stream.next();
    if (done) throw new Error("Expected a complete snapshot");
    if (value.event?.case === "snapshotComplete") return events;
    events.push(value.event);
  }
}

test("captures requests and runners before yielding the initial snapshot", async () => {
  const organizationId = MOCK_IDS.acme;
  const request = { organizationId, requestId: MOCK_IDS.queuedAgent };
  const runner = { organizationId, runnerId: MOCK_IDS.warmRunner };
  const { stream, work } = openWatch(organizationId);
  const originalRequest = structuredClone(
    (await work.getRequest(request)).request,
  );
  const originalRunner = structuredClone((await work.getRunner(runner)).runner);

  try {
    await expect(stream.next()).resolves.toMatchObject({
      done: false,
      value: {
        event: {
          case: "request",
          value: { requestId: MOCK_IDS.queuedJob, state: RequestState.QUEUED },
        },
      },
    });

    await work.cancelRequest(request);
    await work.deleteRunner(runner);

    expect(await readSnapshot(stream)).toEqual([
      { case: "request", value: originalRequest },
      {
        case: "request",
        value: expect.objectContaining({ requestId: MOCK_IDS.queuedDev }),
      },
      {
        case: "request",
        value: expect.objectContaining({ requestId: MOCK_IDS.runningJob }),
      },
      {
        case: "runner",
        value: expect.objectContaining({ runnerId: MOCK_IDS.busyRunner }),
      },
      { case: "runner", value: originalRunner },
    ]);
    await expect(stream.next()).resolves.toMatchObject({
      done: false,
      value: {
        event: {
          case: "request",
          value: { requestId: request.requestId, state: RequestState.CANCELED },
        },
      },
    });
    await expect(stream.next()).resolves.toMatchObject({
      done: false,
      value: {
        event: {
          case: "runner",
          value: { runnerId: runner.runnerId, state: RunnerState.DELETED },
        },
      },
    });
  } finally {
    await stream.return?.();
  }
});

test.each([RunnerState.DELETED, RunnerState.DELETING, RunnerState.FAILED])(
  "deleting a runner in state %s preserves history and publishes no update",
  async (state) => {
    const organizationId = MOCK_IDS.acme;
    const runnerId = MOCK_IDS.deletedRunner;
    const request = { organizationId, runnerId };
    const { work, audit, stream, getStoredRunner } = openWatch(organizationId);
    if (state !== RunnerState.DELETED) {
      // Set up states that the default mock does not seed.
      const runner = await getStoredRunner(runnerId);
      runner.state = state;
    }
    const { runner: before } = await work.getRunner(request);
    expect(before).toMatchObject({
      state,
      endReason: RunnerEndReason.WORK_DONE,
      endedAt: expect.any(Object),
    });
    const history = await audit.listAuditEvents({ organizationId });
    await readSnapshot(stream);

    try {
      expect((await work.deleteRunner(request)).runner).toEqual(before);
      expect((await work.getRunner(request)).runner).toEqual(before);
      expect(await audit.listAuditEvents({ organizationId })).toEqual(history);

      const next = stream.next();
      rs.advanceTimersByTime(10_000);
      await expect(next).resolves.toMatchObject({
        value: { event: { case: "keepalive" } },
      });
    } finally {
      await stream.return?.();
    }
  },
);

const organizationId = MOCK_IDS.acme;
const terminalRequestId = "terminal-agent-request";
const terminalRunnerId = "runner-with-terminal-request";
const terminalFixture: MockTransportOptions = {
  workFixtures: [
    {
      organizationId,
      requests: [
        {
          requestId: terminalRequestId,
          runnerId: terminalRunnerId,
          kind: WorkloadKind.AGENT_SESSION,
          state: RequestState.CANCELED,
          endReason: RequestEndReason.CANCELED_BY_USER,
          endedAt: timestampFromMs(Date.UTC(2025, 0, 1)),
        },
      ],
      runners: [
        {
          runnerId: terminalRunnerId,
          requestId: terminalRequestId,
          kind: WorkloadKind.AGENT_SESSION,
          state: RunnerState.BUSY,
        },
      ],
    },
  ],
};

test.each([
  { label: "a warm runner", runnerId: MOCK_IDS.warmRunner },
  {
    label: "a GitHub runner",
    runnerId: MOCK_IDS.busyRunner,
    requestId: MOCK_IDS.runningJob,
  },
  {
    label: "a runner bound to a terminal request",
    runnerId: terminalRunnerId,
    requestId: terminalRequestId,
    options: terminalFixture,
  },
])(
  "deleting $label records one runner update and one audit event across retries",
  async ({ runnerId, requestId, options }) => {
    const firstDeletionTime = Date.UTC(2026, 0, 1);
    rs.setSystemTime(firstDeletionTime);

    const request = { organizationId, runnerId };
    const { work, audit, stream } = openWatch(organizationId, options);
    const readRequest = async () =>
      requestId
        ? (await work.getRequest({ organizationId, requestId })).request
        : undefined;
    const before = structuredClone(await readRequest());
    const history = await audit.listAuditEvents({ organizationId });
    await readSnapshot(stream);

    try {
      const deleted = await work.deleteRunner(request);
      expect(deleted.runner).toMatchObject({
        runnerId,
        state: RunnerState.DELETED,
        endReason: RunnerEndReason.DELETED_BY_USER,
        endedAt: timestampFromMs(firstDeletionTime),
      });
      const update = stream.next();
      rs.advanceTimersByTime(10_000);
      await expect(update).resolves.toMatchObject({
        value: { event: { case: "runner", value: deleted.runner } },
      });

      expect(await readRequest()).toEqual(before);
      expect(await work.deleteRunner(request)).toEqual(deleted);
      expect((await work.getRunner(request)).runner).toEqual(deleted.runner);
      const { events } = await audit.listAuditEvents({ organizationId });
      expect(events[0]).toMatchObject({
        action: AuditAction.RUNNER_DELETED,
        targetKind: AuditTargetKind.RUNNER,
        targetId: runnerId,
      });
      expect(events.slice(1)).toEqual(history.events);

      const next = stream.next();
      rs.advanceTimersByTime(10_000);
      await expect(next).resolves.toMatchObject({
        value: { event: { case: "keepalive" } },
      });
    } finally {
      await stream.return?.();
    }
  },
);

test.each([
  {
    label: "running dev session",
    kind: WorkloadKind.DEV_SESSION,
    requestState: RequestState.RUNNING,
    runnerState: RunnerState.BUSY,
    endState: RequestState.ENDED,
    endReason: RequestEndReason.SESSION_DELETED,
  },
  {
    label: "starting agent session",
    kind: WorkloadKind.AGENT_SESSION,
    requestState: RequestState.STARTING,
    runnerState: RunnerState.PREPARING,
    endState: RequestState.FAILED,
    endReason: RequestEndReason.RUNNER_LOST,
  },
])(
  "deleting the runner for a $label ends the request exactly once",
  async ({ kind, requestState, runnerState, endState, endReason }) => {
    const organizationId = MOCK_IDS.acme;
    const requestId = "bound-request";
    const runnerId = "bound-runner";
    const now = Date.UTC(2026, 0, 1);
    const endedAt = timestampFromMs(now);
    rs.setSystemTime(now);
    const { transport, work, audit, stream } = openWatch(organizationId, {
      workFixtures: [
        {
          organizationId,
          requests: [{ requestId, runnerId, kind, state: requestState }],
          runners: [{ runnerId, requestId, kind, state: runnerState }],
          devSessions:
            kind === WorkloadKind.DEV_SESSION
              ? [
                  {
                    devSessionId: requestId,
                    runnerId,
                    ownerUserId: MOCK_USER,
                    state: requestState,
                  },
                ]
              : [],
        },
      ],
    });
    const deletion = { organizationId, runnerId };
    const request = { organizationId, requestId };
    const list = { organizationId };
    const history = { organizationId, includeEnded: true };
    const auditsBefore = await audit.listAuditEvents(list);
    const eventsMatch = (event: unknown) =>
      expect.objectContaining({ case: "request", value: event });
    const endedRequest = { requestId, state: endState, endReason, endedAt };

    try {
      expect(await readSnapshot(stream)).toContainEqual(
        eventsMatch(
          expect.objectContaining({ requestId, state: requestState }),
        ),
      );
      const deleted = await work.deleteRunner(deletion);
      expect(deleted.runner).toMatchObject({
        runnerId,
        state: RunnerState.DELETED,
        endReason: RunnerEndReason.DELETED_BY_USER,
      });
      const firstUpdate = await stream.next();
      const secondUpdate = stream.next();
      rs.advanceTimersByTime(10_000);
      const updates = [
        firstUpdate.value?.event,
        (await secondUpdate).value?.event,
      ];
      expect(updates).toEqual(
        expect.arrayContaining([
          eventsMatch(expect.objectContaining(endedRequest)),
          { case: "runner", value: deleted.runner },
        ]),
      );

      expect((await work.getRequest(request)).request).toMatchObject(
        endedRequest,
      );
      expect(
        (await work.listRequests(list)).requests.map((r) => r.requestId),
      ).not.toContain(requestId);
      expect((await work.listRequests(history)).requests).toContainEqual(
        expect.objectContaining(endedRequest),
      );

      if (kind === WorkloadKind.DEV_SESSION) {
        const sessions = createClient(DevSessionService, transport);
        const session = {
          devSessionId: requestId,
          state: endState,
          endReason,
          endedAt,
        };
        expect(
          (
            await sessions.getDevSession({
              organizationId,
              devSessionId: requestId,
            })
          ).devSession,
        ).toMatchObject(session);
        expect(
          (await sessions.listDevSessions(list)).devSessions.map(
            (s) => s.devSessionId,
          ),
        ).not.toContain(requestId);
        expect(
          (await sessions.listDevSessions(history)).devSessions,
        ).toContainEqual(expect.objectContaining(session));
      }

      const fresh = work.watchQueue(list)[Symbol.asyncIterator]();
      try {
        expect(await readSnapshot(fresh)).not.toContainEqual(
          eventsMatch(expect.objectContaining({ requestId })),
        );
      } finally {
        await fresh.return?.();
      }

      const auditsAfter = await audit.listAuditEvents(list);
      const [newAudit, ...olderAudits] = auditsAfter.events;
      expect(newAudit).toMatchObject({
        action: AuditAction.RUNNER_DELETED,
        targetKind: AuditTargetKind.RUNNER,
        targetId: runnerId,
      });
      expect(olderAudits).toEqual(auditsBefore.events);

      rs.setSystemTime(now + 1_000);
      expect(await work.deleteRunner(deletion)).toEqual(deleted);
      expect((await work.getRequest(request)).request).toMatchObject(
        endedRequest,
      );
      expect(await audit.listAuditEvents(list)).toEqual(auditsAfter);
      const next = stream.next();
      rs.advanceTimersByTime(10_000);
      await expect(next).resolves.toMatchObject({
        value: { event: { case: "keepalive" } },
      });
    } finally {
      await stream.return?.();
    }
  },
);

test("releases each wait after a keepalive or queue event", async () => {
  const { stream, cancelRequest, pendingAbortListeners } = openWatch(
    MOCK_IDS.acme,
  );
  const snapshot: string[] = [];
  for (;;) {
    const { value, done } = await stream.next();
    if (done) throw new Error("Expected a complete snapshot");
    if (value.event?.case === "snapshotComplete") break;
    snapshot.push(value.event?.case ?? "");
  }
  expect(snapshot).toEqual([
    "request",
    "request",
    "request",
    "request",
    "runner",
    "runner",
  ]);

  for (const requestId of [MOCK_IDS.queuedJob, MOCK_IDS.queuedAgent]) {
    const idle = stream.next();
    expect(pendingAbortListeners()).toBe(1);
    expect(rs.getTimerCount()).toBe(1);
    rs.advanceTimersByTime(10_000);
    await expect(idle).resolves.toMatchObject({
      done: false,
      value: { event: { case: "keepalive" } },
    });
    expect(pendingAbortListeners()).toBe(0);
    expect(rs.getTimerCount()).toBe(0);

    const nextEvent = stream.next();
    expect(pendingAbortListeners()).toBe(1);
    await cancelRequest(requestId);
    await expect(nextEvent).resolves.toMatchObject({
      done: false,
      value: {
        event: {
          case: "request",
          value: { requestId, state: RequestState.CANCELED },
        },
      },
    });
    expect(pendingAbortListeners()).toBe(0);
    expect(rs.getTimerCount()).toBe(0);
  }
  await stream.return?.();
});

test.each([false, true])(
  "cleans up the watcher on abort (before the idle read: %s)",
  async (abortBeforeRead) => {
    const { stream, context, pendingAbortListeners } = openWatch(
      MOCK_IDS.globex,
    );
    const registerWatchers = rs.spyOn(Map.prototype, "set");
    const snapshot = stream.next();
    const registration = registerWatchers.mock.calls.find(
      ([organizationId]) => organizationId === MOCK_IDS.globex,
    );
    registerWatchers.mockRestore();
    const watcherSet = registration?.[1] as Set<unknown> | undefined;
    await expect(snapshot).resolves.toMatchObject({
      done: false,
      value: { event: { case: "snapshotComplete" } },
    });
    expect(watcherSet?.size).toBe(1);

    if (abortBeforeRead) context.abort();
    const idle = stream.next();
    expect(pendingAbortListeners()).toBe(abortBeforeRead ? 0 : 1);
    expect(rs.getTimerCount()).toBe(abortBeforeRead ? 0 : 1);
    if (!abortBeforeRead) context.abort();

    await expect(idle).resolves.toMatchObject({ done: true });
    expect(pendingAbortListeners()).toBe(0);
    expect(rs.getTimerCount()).toBe(0);
    expect(watcherSet?.size).toBe(0);
  },
);

test("releases the last wait on a configured clean end", async () => {
  const { stream, context, pendingAbortListeners } = openWatch(
    MOCK_IDS.globex,
    {
      watchQueueEndsAfterMs: 20,
    },
  );
  await expect(stream.next()).resolves.toMatchObject({
    done: false,
    value: { event: { case: "snapshotComplete" } },
  });
  const end = stream.next();
  expect(pendingAbortListeners()).toBe(1);
  expect(rs.getTimerCount()).toBe(1);
  rs.advanceTimersByTime(20);
  await expect(end).resolves.toMatchObject({ done: true });
  expect(context.signal.aborted).toBe(false);
  expect(pendingAbortListeners()).toBe(0);
  expect(rs.getTimerCount()).toBe(0);
});
