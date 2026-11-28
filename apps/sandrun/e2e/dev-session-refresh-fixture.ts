// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
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
  RequestEndReason,
  RequestState,
  RunnerEndReason,
  RunnerState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import {
  DevSessionSchema,
  DevSessionService,
} from "@delino/sandrun-protos/v1/dev_session_pb";
import { RunnerSchema, WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { MOCK_IDS, MOCK_USER } from "../src/mock/mock-api.ts";

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
    error: "Fixture rejected cancellation.",
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

export function devSessionRefreshFixture(
  mock: Transport,
  mode: DevSessionRefreshMode,
) {
  const work = createClient(WorkService, mock);
  const sessions = createClient(DevSessionService, mock);
  const reads: boolean[] = [];
  const session = create(DevSessionSchema, {
    devSessionId: "0193b000-0000-7000-8000-0000000000e6",
    organizationId: MOCK_IDS.acme,
    runnerProfileId: MOCK_IDS.devProfile,
    runnerProfileName: "windows-dev",
    ownerUserId: MOCK_USER,
    label: DEV_SESSION_REFRESH_CASES[1].session,
    runnerId: "0193b000-0000-7000-8000-0000000000f4",
    guestOs: GuestOs.WINDOWS,
    state: RequestState.RUNNING,
    endReason: RequestEndReason.UNSPECIFIED,
    queuedAt: timestampNow(),
    startedAt: timestampNow(),
  });
  const runner = create(RunnerSchema, {
    runnerId: session.runnerId,
    requestId: session.devSessionId,
    organizationId: session.organizationId,
    runnerProfileId: session.runnerProfileId,
    kind: WorkloadKind.DEV_SESSION,
    guestOs: session.guestOs,
    state: RunnerState.BUSY,
    endReason: RunnerEndReason.UNSPECIFIED,
    createdAt: session.startedAt,
  });
  const fixture = createRouterTransport((router) => {
    router.service(DevSessionService, {
      async listDevSessions(req) {
        const response = await sessions.listDevSessions(req);
        if (req.organizationId === session.organizationId) {
          reads.push(req.includeEnded);
          if (req.includeEnded || session.state === RequestState.RUNNING) {
            response.devSessions.push({ ...session });
          }
        }
        return response;
      },
    });
    router.service(WorkService, {
      cancelRequest(req) {
        if (mode === "fail-cancel" && req.requestId === MOCK_IDS.queuedDev) {
          throw new ConnectError(
            "Fixture rejected cancellation.",
            Code.FailedPrecondition,
          );
        }
        return work.cancelRequest(req);
      },
      async listRunners(req) {
        const response = await work.listRunners(req);
        if (
          req.organizationId === runner.organizationId &&
          (req.includeEnded || runner.state === RunnerState.BUSY)
        ) {
          response.runners.push({ ...runner });
        }
        return response;
      },
      deleteRunner(req) {
        if (req.runnerId !== runner.runnerId) return work.deleteRunner(req);
        if (mode === "fail-delete") {
          throw new ConnectError(
            "Fixture rejected deletion.",
            Code.FailedPrecondition,
          );
        }
        // User deletion ends the linked dev-session request on the server.
        const endedAt = timestampNow();
        Object.assign(session, {
          state: RequestState.ENDED,
          endReason: RequestEndReason.SESSION_DELETED,
          endedAt,
        });
        Object.assign(runner, {
          state: RunnerState.DELETED,
          endReason: RunnerEndReason.DELETED_BY_USER,
          endedAt,
        });
        return { runner };
      },
    });
  });
  const methods = new Set([
    DevSessionService.method.listDevSessions.name,
    WorkService.method.cancelRequest.name,
    WorkService.method.listRunners.name,
    WorkService.method.deleteRunner.name,
  ]);
  const transport: Transport = {
    ...mock,
    unary: (method, ...args) =>
      (methods.has(method.name) ? fixture : mock).unary(method, ...args),
  };
  return { transport, reads };
}
