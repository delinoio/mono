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
import {
  SandboxState,
  SnapshotExportState,
  SnapshotState,
} from "ownsand-sdk/gen/common_pb";

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
