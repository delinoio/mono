// SPDX-License-Identifier: Apache-2.0
import {
  GuestOs,
  RequestState,
  RunnerState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import {
  MOCK_IDS,
  MOCK_USER,
  type MockTransportOptions,
} from "../src/mock/mock-api.ts";

export const DEV_SESSION_DELETION_FIXTURE = "sandrun-dev-session-deletion";
export const DELETION_SESSION_ID = "fixture-dev";
export const DELETION_SESSION_LABEL = "Fixture dev session";
export const DELETION_RUNNER_ID = "dev-runner";

export function devSessionDeletionFixtures(
  mode: "starting" | "running",
): MockTransportOptions["workFixtures"] {
  const starting = mode === "starting";
  const state = starting ? RequestState.STARTING : RequestState.RUNNING;
  const runner = {
    runnerId: DELETION_RUNNER_ID,
    runnerProfileId: MOCK_IDS.devProfile,
    guestOs: GuestOs.LINUX,
    ownsandDeviceId: MOCK_IDS.macDevice,
  };
  return [
    {
      organizationId: MOCK_IDS.acme,
      devSessions: [
        {
          ...runner,
          devSessionId: DELETION_SESSION_ID,
          ownerUserId: MOCK_USER,
          label: DELETION_SESSION_LABEL,
          state,
        },
      ],
      requests: [
        {
          requestId: DELETION_SESSION_ID,
          runnerId: runner.runnerId,
          runnerProfileId: runner.runnerProfileId,
          kind: WorkloadKind.DEV_SESSION,
          state,
        },
      ],
      runners: [
        {
          ...runner,
          requestId: DELETION_SESSION_ID,
          kind: WorkloadKind.DEV_SESSION,
          state: starting ? RunnerState.PREPARING : RunnerState.BUSY,
        },
      ],
    },
  ];
}
