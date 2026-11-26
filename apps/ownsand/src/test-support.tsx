// SPDX-License-Identifier: Apache-2.0
// Shared helpers for the console tests that live outside `console.test.tsx`.
import { act, render } from "@testing-library/react";
import type { DescMethodUnary } from "@bufbuild/protobuf";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { QueryClient } from "@tanstack/react-query";
import { Console } from "./App.tsx";
import { createQueryClient } from "./api.ts";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { AuthMode, type ConsoleConfig } from "./config.ts";
import { createMockTransport, MOCK_USER } from "./mock/mock-api.ts";

/** Testing Library's timeout when a query or wait has no timeout option. */
export const DEFAULT_WAIT_MS = 1_000;

/** Add this allowance for setup and actions to the longest sequence of waits. */
export const TEST_SETUP_MS = 5_000;

export const testConfig: ConsoleConfig = {
  apiOrigin: "https://api.test",
  authMode: AuthMode.Mock,
  logto: undefined,
};

/** Renders the console at `path` for the signed-in mock user. */
export function renderConsole(
  path: string,
  options: {
    transport?: Transport;
    queryClient?: QueryClient;
    /** The signed-in user; the mock user by default. */
    userId?: string;
  } = {},
) {
  window.history.replaceState(null, "", path);
  const auth: Auth = {
    isLoading: false,
    isAuthenticated: true,
    userId: options.userId ?? MOCK_USER,
    signIn: async () => {},
    signInFailed: false,
    signOut: async () => {},
    getAccessToken: async () => "token",
  };
  const queryClient =
    options.queryClient ??
    new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
  render(
    <AuthContextProvider value={auth}>
      <Console
        config={testConfig}
        transport={options.transport ?? createMockTransport()}
        queryClient={queryClient}
      />
    </AuthContextProvider>,
  );
}

/** Lets pending promises and timers run inside `act`. */
export async function settle(ms = 30) {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}

/** A request that a test holds until it decides how the request ends. */
export interface HeldCall {
  /** The request message. */
  input: unknown;
  /** Lets the request succeed, or fails it when `error` is set. */
  settle(error?: Error): void;
}

/**
 * A transport that holds every request of the named methods that
 * `shouldHold` accepts, until the test settles it. Held requests are
 * recorded per method in `calls`; `seen` counts every request, held or not.
 */
export function holdingTransport(
  methods: string[],
  shouldHold: (method: string) => boolean = () => true,
  mock: Transport = createMockTransport(),
) {
  const calls: Record<string, HeldCall[]> = {};
  const seen: Record<string, number> = {};
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      const name = method.name;
      seen[name] = (seen[name] ?? 0) + 1;
      if (methods.includes(name)) {
        if (shouldHold(name)) {
          const held = (calls[name] ??= []);
          const error = await new Promise<Error | undefined>((resolve) => {
            held.push({ input: rest[3], settle: resolve });
          });
          if (error !== undefined) throw error;
        }
      }
      return mock.unary(method, ...rest);
    },
  };
  return { transport, calls, seen };
}

/** Refetches one list from the first page with the console's retry policy. */
export function fullListRefresh(
  method: DescMethodUnary,
  mock: Transport = createMockTransport(),
) {
  let failure: Code | undefined;
  const failedTokens: string[] = [];
  const seen: Record<string, number> = {};
  const transport: Transport = {
    ...mock,
    async unary(rpc, ...rest) {
      seen[rpc.name] = (seen[rpc.name] ?? 0) + 1;
      if (rpc.name === method.name && failure !== undefined) {
        failedTokens.push((rest[3] as { pageToken: string }).pageToken);
        throw new ConnectError("the refresh failed", failure);
      }
      return mock.unary(rpc, ...rest);
    },
  };
  const queryClient = createQueryClient();
  const defaults = queryClient.getDefaultOptions();
  queryClient.setDefaultOptions({
    ...defaults,
    queries: { ...defaults.queries, retryDelay: 0 },
  });
  const queryKey = createConnectQueryKey({
    schema: method,
    cardinality: "infinite",
  });
  const refetch = async (code?: Code) => {
    failure = code;
    await act(() => queryClient.refetchQueries({ queryKey }));
  };
  return { transport, queryClient, refetch, failedTokens, seen };
}
