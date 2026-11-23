// SPDX-License-Identifier: Apache-2.0
import { QueryClient } from "@tanstack/react-query";
import { StrictMode } from "react";
import { act, render } from "@testing-library/react";
import { Accounts } from "../App.tsx";
import { type Auth, AuthContextProvider } from "../auth.tsx";
import type { AccountsConfig } from "../config.ts";
import type { Checkout } from "../paddle.tsx";
import { createFakeApi, type FakeApi } from "./fake-api.ts";

export const TEST_CONFIG: AccountsConfig = {
  apiOrigin: "https://accounts.api.test",
  logto: { endpoint: "https://logto.test", appId: "app", scopes: [] },
  paddle: undefined,
};

/** A checkout that records the transactions it opens. */
export function fakeCheckout(): Checkout & {
  opened: string[];
  /** The signal of every `open` call, in call order. */
  signals: (AbortSignal | undefined)[];
  close(): void;
} {
  let onClosed: (() => void) | undefined;
  const opened: string[] = [];
  const signals: (AbortSignal | undefined)[] = [];
  return {
    opened,
    signals,
    async open(transactionId, closed, signal) {
      signals.push(signal);
      // Like the Paddle adapter, an aborted request opens nothing.
      if (signal?.aborted) return;
      opened.push(transactionId);
      onClosed = closed;
    },
    close() {
      onClosed?.();
    },
  };
}

/** Renders the app at `path` against a fake API. */
export function renderApp(
  path: string,
  options: {
    api?: FakeApi;
    signedIn?: boolean;
    checkout?: Checkout;
    config?: AccountsConfig;
    /** Replaces the access token source of the signed-in user. */
    getAccessToken?: () => Promise<string | undefined>;
    /**
     * Uses the app's own API transport, which reads `getAccessToken`, instead
     * of the fake API transport.
     */
    appTransport?: boolean;
    /** Reports a sign-in that failed before the redirect. */
    signInFailed?: boolean;
    /** Renders inside `StrictMode`, as `index.tsx` does. */
    strict?: boolean;
  } = {},
) {
  window.history.replaceState(null, "", path);
  const api = options.api ?? createFakeApi();
  const signIns: number[] = [];
  const signedIn = options.signedIn ?? true;
  const auth: Auth = {
    isLoading: false,
    isAuthenticated: signedIn,
    userId: signedIn ? api.userId : undefined,
    signIn: async () => {
      signIns.push(1);
    },
    signInFailed: options.signInFailed ?? false,
    signOut: async () => {},
    getAccessToken: options.getAccessToken ?? (async () => "token"),
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const app = (
    <AuthContextProvider value={auth}>
      <Accounts
        config={options.config ?? TEST_CONFIG}
        transport={options.appTransport ? undefined : api.transport}
        queryClient={queryClient}
        checkout={options.checkout}
      />
    </AuthContextProvider>
  );
  render(options.strict ? <StrictMode>{app}</StrictMode> : app);
  return { api, signIns };
}

/** Lets pending requests and effects finish. */
export async function settle(ms = 30) {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}
