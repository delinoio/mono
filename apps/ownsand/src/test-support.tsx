// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { AuthContextProvider, AuthStatus, type AuthApi } from "./auth/auth.tsx";
import { RouterProvider } from "./router.tsx";

/** Shared helpers for unit tests; not imported by application code. */

export function fakeAuth(overrides: Partial<AuthApi> = {}): AuthApi {
  return {
    status: AuthStatus.SignedIn,
    userId: "user-self",
    signIn: () => Promise.resolve(),
    signOut: () => Promise.resolve(),
    getAccessToken: () => Promise.resolve("unit-test-token"),
    Callback: () => null,
    ...overrides,
  };
}

export function TestProviders({
  transport,
  auth,
  children,
  onPathChange,
}: {
  readonly transport: Transport;
  readonly auth: AuthApi;
  readonly children: ReactNode;
  readonly onPathChange?: (pathname: string) => void;
}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return (
    <RouterProvider onPathChange={onPathChange}>
      <AuthContextProvider value={auth}>
        <TransportProvider transport={transport}>
          <QueryClientProvider client={queryClient}>
            {children}
          </QueryClientProvider>
        </TransportProvider>
      </AuthContextProvider>
    </RouterProvider>
  );
}
