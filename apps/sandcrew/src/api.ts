// SPDX-License-Identifier: Apache-2.0
import type {
  DescMessage,
  DescMethodUnary,
  DescService,
  MessageInitShape,
  MessageShape,
} from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import {
  callUnaryMethod,
  createConnectQueryKey,
  useTransport,
} from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  QueryClient,
  type QueryKey,
  useQuery,
  type UseQueryResult,
} from "@tanstack/react-query";

/** The largest page that the control plane returns. */
export const MAX_PAGE_SIZE = 200;

/** The page size of the session list. */
export const SESSION_PAGE_SIZE = 50;

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

/** A page of a list response: its items and the continuation token. */
export interface Page<T> {
  items: readonly T[];
  nextPageToken: string;
}

/** The query key of a complete list. Service invalidation also matches it. */
export function allPagesKey<I extends DescMessage, O extends DescMessage>(
  method: DescMethodUnary<I, O>,
  input: MessageInitShape<I>,
  transport: Transport,
): QueryKey {
  return [
    ...createConnectQueryKey({
      schema: method,
      input,
      transport,
      cardinality: "finite",
    }),
    "all-pages",
  ] as unknown as QueryKey;
}

/**
 * Loads every page of a small list (credentials, secrets, profiles,
 * organizations, push subscriptions) into one query with the largest page
 * size, following each continuation token.
 */
export function useAllPages<I extends DescMessage, O extends DescMessage, T>(
  method: DescMethodUnary<I, O>,
  input: MessageInitShape<I>,
  page: (response: MessageShape<O>) => Page<T>,
  options: { enabled?: boolean } = {},
): UseQueryResult<T[]> {
  const transport = useTransport();
  return useQuery({
    queryKey: allPagesKey(method, input, transport),
    enabled: options.enabled ?? true,
    queryFn: async ({ signal }) => {
      const items: T[] = [];
      let pageToken = "";
      // A server that returns the same token again would loop forever.
      const seen = new Set<string>();
      for (;;) {
        const response = await callUnaryMethod(
          transport,
          method,
          {
            ...input,
            pageSize: MAX_PAGE_SIZE,
            pageToken,
          } as MessageInitShape<I>,
          { signal },
        );
        const { items: pageItems, nextPageToken } = page(response);
        items.push(...pageItems);
        if (nextPageToken === "" || seen.has(nextPageToken)) return items;
        seen.add(nextPageToken);
        pageToken = nextPageToken;
      }
    },
  });
}

/**
 * A new UUID v7 in canonical text form, for the `client_message_id` of an
 * input RPC.
 */
export function uuidV7(now = Date.now()): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  let ms = now;
  for (let i = 5; i >= 0; i--) {
    bytes[i] = ms % 256;
    ms = Math.floor(ms / 256);
  }
  bytes[6] = (bytes[6]! & 0x0f) | 0x70;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join(
    "",
  );
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
