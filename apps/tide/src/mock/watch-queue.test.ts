// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import {
  createConnectRouter,
  createHandlerContext,
  createRouterTransport,
  type ServiceImpl,
} from "@connectrpc/connect";
import * as actualConnect from "@connectrpc/connect" with {
  rstest: "importActual",
};
import { RequestState } from "@delino/tide-protos/v1/common_pb";
import {
  CancelRequestRequestSchema,
  WatchQueueRequestSchema,
  WorkService,
} from "@delino/tide-protos/v1/work_pb";
import { afterEach, beforeEach, expect, rs, test } from "@rstest/core";
import {
  createMockTransport,
  MOCK_IDS,
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
  createMockTransport(options);
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
