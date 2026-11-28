// SPDX-License-Identifier: Apache-2.0
import { timestampNow } from "@bufbuild/protobuf/wkt";
import {
  Code,
  ConnectError,
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import {
  GuestOs,
  RequestState,
  RunnerState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";
import {
  MOCK_IDS,
  MOCK_USER,
  type MockTransportOptions,
} from "../src/mock/mock-api.ts";

export const DEV_SESSION_REFRESH_FIXTURE = "sandrun-e2e-dev-session-refresh";
export const DEV_SESSION_REFRESH_CASES = [
  {
    page: "Requests",
    workRow: "0193b000…00e3",
    action: "Cancel",
    dialog: "Cancel this request?",
    confirm: "Cancel request",
    session: "msvc build",
    activeState: "Queued",
    endedState: "Canceled",
    reason: "Canceled by user",
    failureMode: "fail-cancel",
    error: "SandRun is not available right now. Try again in a moment.",
    screenshot: "canceled-request",
  },
  {
    page: "Runners",
    workRow: "0193b000…00f4",
    action: "Delete",
    dialog: "Delete this runner?",
    confirm: "Delete runner",
    session: "active compiler session",
    activeState: "Running",
    endedState: "Ended",
    reason: "Session deleted",
    failureMode: "fail-delete",
    error: "Fixture rejected deletion.",
    screenshot: "deleted-runner",
  },
] as const;
export type DevSessionRefreshMode = "success" | "fail-cancel" | "fail-delete";

const devSessionId = "0193b000-0000-7000-8000-0000000000e6";
const runnerId = "0193b000-0000-7000-8000-0000000000f4";

function devSessionRefreshWork(): NonNullable<
  MockTransportOptions["workFixtures"]
>[number] {
  const startedAt = timestampNow();
  const profile = {
    runnerProfileId: MOCK_IDS.devProfile,
    guestOs: GuestOs.WINDOWS,
  };
  return {
    organizationId: MOCK_IDS.acme,
    requests: [
      {
        ...profile,
        requestId: devSessionId,
        runnerId,
        ownerUserId: MOCK_USER,
        kind: WorkloadKind.DEV_SESSION,
        state: RequestState.RUNNING,
        queuedAt: startedAt,
        startedAt,
      },
    ],
    runners: [
      {
        ...profile,
        runnerId,
        requestId: devSessionId,
        kind: WorkloadKind.DEV_SESSION,
        state: RunnerState.BUSY,
        createdAt: startedAt,
      },
    ],
    devSessions: [
      {
        ...profile,
        devSessionId,
        runnerId,
        runnerProfileName: "windows-dev",
        ownerUserId: MOCK_USER,
        label: DEV_SESSION_REFRESH_CASES[1].session,
        state: RequestState.RUNNING,
        queuedAt: startedAt,
        startedAt,
      },
    ],
  };
}

export function devSessionRefreshFixture(
  createMock: (options?: MockTransportOptions) => Transport,
  mode: DevSessionRefreshMode,
  options?: MockTransportOptions,
) {
  const mock = createMock({
    ...options,
    workFixtures: [...(options?.workFixtures ?? []), devSessionRefreshWork()],
  });
  const work = createClient(WorkService, mock);
  const sessions = createClient(DevSessionService, mock);
  const reads: boolean[] = [];
  const fixture = createRouterTransport((router) => {
    router.service(DevSessionService, {
      async listDevSessions(req) {
        const response = await sessions.listDevSessions(req);
        if (req.organizationId === MOCK_IDS.acme) {
          reads.push(req.includeEnded);
        }
        return response;
      },
    });
    router.service(WorkService, {
      cancelRequest(req) {
        if (mode === "fail-cancel" && req.requestId === MOCK_IDS.queuedDev) {
          throw new ConnectError(
            "Fixture rejected cancellation.",
            Code.Unavailable,
          );
        }
        return work.cancelRequest(req);
      },
      deleteRunner(req) {
        if (mode === "fail-delete" && req.runnerId === runnerId) {
          throw new ConnectError(
            "Fixture rejected deletion.",
            Code.FailedPrecondition,
          );
        }
        return work.deleteRunner(req);
      },
    });
  });
  const methods = new Set([
    DevSessionService.method.listDevSessions.name,
    WorkService.method.cancelRequest.name,
    WorkService.method.deleteRunner.name,
  ]);
  const transport: Transport = {
    ...mock,
    unary: (method, ...args) =>
      (methods.has(method.name) ? fixture : mock).unary(method, ...args),
  };
  return { transport, reads };
}
