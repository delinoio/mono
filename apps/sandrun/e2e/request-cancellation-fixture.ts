// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { RequestState } from "@delino/sandrun-protos/v1/common_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const REQUEST_CANCELLATION_FIXTURE = "sandrun-e2e-request-cancellation";

type Outcome = "running" | "ended" | "requeued" | "unavailable";

export function requestCancellationFixture(mock: Transport) {
  const work = createClient(WorkService, mock);
  let completeCancellation!: (outcome: Outcome) => void;
  const cancel = new Promise<Outcome>((resolve) => {
    completeCancellation = resolve;
  });
  let releaseRefresh!: () => void;
  const refresh = new Promise<void>((resolve) => {
    releaseRefresh = resolve;
  });
  const state = { cancellations: [] as string[], refreshCalls: 0 };
  let transition: Exclude<Outcome, "unavailable"> | undefined;
  let failRefresh = false;
  let firstJobCancellation = true;

  const fixture = createRouterTransport((router) => {
    router.service(WorkService, {
      async cancelRequest(request) {
        state.cancellations.push(request.requestId);
        if (request.requestId !== MOCK_IDS.queuedJob) {
          return work.cancelRequest(request);
        }
        if (firstJobCancellation) {
          firstJobCancellation = false;
          const outcome = await cancel;
          if (outcome === "unavailable") {
            throw new ConnectError(
              "Cancel temporarily unavailable",
              Code.Unavailable,
            );
          }
          transition = outcome;
        } else if (transition === undefined || transition === "requeued") {
          return work.cancelRequest(request);
        }
        throw new ConnectError(
          "only a QUEUED request can be canceled",
          Code.FailedPrecondition,
        );
      },
      async listRequests(request, { signal }) {
        const response = await work.listRequests(request, { signal });
        if (transition === undefined) return response;
        state.refreshCalls += 1;
        await refresh;
        if (failRefresh) {
          throw new ConnectError(
            "List temporarily unavailable",
            Code.Unavailable,
          );
        }
        const nextState = {
          running: RequestState.RUNNING,
          ended: RequestState.ENDED,
          requeued: RequestState.QUEUED,
        }[transition];
        response.requests = response.requests.flatMap((row) => {
          if (row.requestId !== MOCK_IDS.queuedJob) return [row];
          if (
            (nextState === RequestState.ENDED && !request.includeEnded) ||
            (request.states.length > 0 && !request.states.includes(nextState))
          ) {
            return [];
          }
          return [
            { ...row, state: nextState, startAttempts: row.startAttempts + 1 },
          ];
        });
        return response;
      },
    });
  });
  const transport: Transport = {
    ...mock,
    unary(method, ...args) {
      const selected =
        method.name === "CancelRequest" || method.name === "ListRequests"
          ? fixture
          : mock;
      return selected.unary(method, ...args);
    },
  };
  return {
    transport,
    state,
    completeCancellation,
    completeRefresh(fail = false) {
      failRefresh = fail;
      releaseRefresh();
    },
  };
}
