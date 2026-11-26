// SPDX-License-Identifier: Apache-2.0
import type { DescService } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import {
  createConnectQueryKey,
  useInfiniteQuery,
} from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  type AgentIntegration,
  AgentIntegrationService,
} from "@delino/sandrun-protos/v1/agent_integration_pb";
import { QueryClient } from "@tanstack/react-query";
import { useEffect, useMemo } from "react";

/** Page size of the console's lists. */
export const PAGE_SIZE = 50;

/** The largest page that the control plane returns. */
export const MAX_PAGE_SIZE = 200;

const EMPTY_INTEGRATIONS: readonly AgentIntegration[] = [];

// A refetch starts from the first page. Keep the continuation error until
// the user retries its cursor or a mutation invalidates the query.
function refetchUnlessContinuationFailed(query: {
  state: {
    status: string;
    fetchMeta: { fetchMore?: { direction: string } } | null;
  };
}): boolean {
  const { status, fetchMeta } = query.state;
  return status !== "error" || fetchMeta?.fetchMore?.direction !== "forward";
}

/** Loads all integration pages into one query per organization. */
export function useAgentIntegrations(organizationId: string) {
  const {
    data,
    error,
    fetchNextPage,
    hasNextPage,
    isFetching,
    isFetchNextPageError,
    isPending,
    refetch,
  } = useInfiniteQuery(
    AgentIntegrationService.method.listAgentIntegrations,
    { organizationId, pageSize: MAX_PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
      refetchOnMount: refetchUnlessContinuationFailed,
      refetchOnReconnect: refetchUnlessContinuationFailed,
      refetchOnWindowFocus: refetchUnlessContinuationFailed,
    },
  );
  useEffect(() => {
    if (hasNextPage && !isFetching && !error) {
      void fetchNextPage({ cancelRefetch: false });
    }
  }, [data, error, fetchNextPage, hasNextPage, isFetching]);

  return {
    integrations: useMemo(
      () =>
        data?.pages.flatMap((page) => page.integrations) ?? EMPTY_INTEGRATIONS,
      [data],
    ),
    hasNextPage,
    isLoadingMore: hasNextPage && !error,
    query: {
      data,
      error,
      isFetching,
      isFetchNextPageError,
      isPending,
      refetch: () => refetch({ cancelRefetch: false }),
      retryNextPage: () => void fetchNextPage({ cancelRefetch: false }),
    },
  };
}

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
