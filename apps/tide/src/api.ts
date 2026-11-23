// SPDX-License-Identifier: Apache-2.0
import type { DescService } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import { QueryClient } from "@tanstack/react-query";

/**
 * The ListOrganizations request of the console. The control plane returns 50
 * organizations for an empty request, so every surface sends this one input
 * to read the first 200 and to share one React Query key.
 */
export const LIST_ORGANIZATIONS_REQUEST = { pageSize: 200 } as const;

/** Page size of the console's lists. */
export const PAGE_SIZE = 50;

/** The largest page that the control plane returns. */
export const MAX_PAGE_SIZE = 200;

/**
 * The Connect transport of the console: binary Connect protocol to the
 * absolute API origin, with the Logto access token of the signed-in user.
 * It refuses to send the token to any other origin.
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
