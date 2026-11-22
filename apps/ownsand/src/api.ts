// SPDX-License-Identifier: Apache-2.0
import { createConnectQueryKey } from "@connectrpc/connect-query";
import type { DescService } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { QueryClient } from "@tanstack/react-query";
import { BillingService } from "@delino/ownsand-protos/v1/billing_pb";
import {
  BillingReason,
  ErrorDetailSchema,
  SandboxState,
  SnapshotExportState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";

/**
 * The ListOrganizations request of the console. The control plane returns 50
 * organizations for an empty request, so every surface sends this one input
 * to read pages of the documented maximum of 200 and to share one React
 * Query key. `useOrganizations` follows the continuation tokens.
 */
export const LIST_ORGANIZATIONS_REQUEST = {
  pageSize: 200,
  pageToken: "",
} as const;

/**
 * The Connect transport of the console: binary Connect protocol to the
 * absolute API origin, with the Logto access token of the signed-in user.
 */
export function createApiTransport(
  origin: string,
  getAccessToken: () => Promise<string | undefined>,
): Transport {
  const auth: Interceptor = (next) => async (req) => {
    if (new URL(req.url).origin !== origin) {
      throw new ConnectError(
        "refusing to send a token to another origin",
        Code.PermissionDenied,
      );
    }
    const token = await getAccessToken();
    if (token === undefined) {
      throw new ConnectError("not signed in", Code.Unauthenticated);
    }
    req.header.set("Authorization", `Bearer ${token}`);
    return next(req);
  };
  return createConnectTransport({
    baseUrl: origin,
    useBinaryFormat: true,
    interceptors: [auth],
  });
}

/** The React Query client: retries only transient read failures. */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        retry: (failures, error) =>
          failures < 2 &&
          error instanceof ConnectError &&
          error.code === Code.Unavailable,
      },
      mutations: { retry: false },
    },
  });
}

/**
 * How often a page refetches while a sandbox or snapshot it shows is in an
 * asynchronous state. The control plane returns from create, delete,
 * snapshot and export before the agent finishes, and the console has no
 * subscription, so it polls until the state settles.
 */
export const POLL_INTERVAL_MS = 3_000;

/** Whether a sandbox state changes without a user action. */
export function sandboxSettling(sandbox: { state: SandboxState }): boolean {
  return (
    sandbox.state === SandboxState.CREATING ||
    sandbox.state === SandboxState.DELETING
  );
}

/** Whether a snapshot or its export changes without a user action. */
export function snapshotSettling(snapshot: {
  state: SnapshotState;
  exportState: SnapshotExportState;
}): boolean {
  return (
    snapshot.state === SnapshotState.CREATING ||
    snapshot.state === SnapshotState.DELETING ||
    snapshot.exportState === SnapshotExportState.EXPORTING
  );
}

/** Invalidates every cached query of the given services. */
export function invalidateServices(
  queryClient: QueryClient,
  ...services: DescService[]
): Promise<void> {
  return Promise.all(
    services.map((schema) =>
      queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema, cardinality: undefined }),
      }),
    ),
  ).then(() => undefined);
}

/**
 * The billing reason of an RPC error that billing enforcement rejected: a
 * `resource_exhausted` error with an `ErrorDetail` whose `billing_reason`
 * is set. A `resource_exhausted` error without one is a rate limit or a
 * capacity error and returns undefined. Callers never retry a billing
 * error.
 */
export function billingReasonOf(error: unknown): BillingReason | undefined {
  if (!(error instanceof ConnectError)) return undefined;
  if (error.code !== Code.ResourceExhausted) return undefined;
  for (const detail of error.findDetails(ErrorDetailSchema)) {
    if (detail.billingReason !== BillingReason.UNSPECIFIED) {
      return detail.billingReason;
    }
  }
  return undefined;
}

/**
 * Reads the billing status again after any query or mutation of the client
 * fails with a billing reason, so the billing banner shows the phase that
 * rejected the call. Returns the function that stops watching.
 */
export function watchBillingErrors(queryClient: QueryClient): () => void {
  const refresh = (error: unknown) => {
    if (billingReasonOf(error) === undefined) return;
    void queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({
        schema: BillingService.method.getBillingStatus,
        cardinality: undefined,
      }),
    });
  };
  const stopQueries = queryClient.getQueryCache().subscribe((event) => {
    if (event.type === "updated" && event.action.type === "error") {
      refresh(event.action.error);
    }
  });
  const stopMutations = queryClient.getMutationCache().subscribe((event) => {
    if (event.type === "updated" && event.action.type === "error") {
      refresh(event.action.error);
    }
  });
  return () => {
    stopQueries();
    stopMutations();
  };
}
