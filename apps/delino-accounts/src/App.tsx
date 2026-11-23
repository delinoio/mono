// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { ToastRegion } from "delino-ui/toast";
import { type ReactNode, useCallback, useMemo, useState } from "react";
import { createApiTransport, createQueryClient } from "./api.ts";
import {
  AuthContextProvider,
  LogtoAuthProvider,
  NOT_CONFIGURED,
  useAuth,
} from "./auth.tsx";
import { Page } from "./components/shell.tsx";
import { PageHeader, SignInFailureNotice } from "./components/ui.tsx";
import type { AccountsConfig } from "./config.ts";
import { pendingInvitation } from "./invitation.ts";
import {
  type Checkout,
  CheckoutProvider,
  createPaddleCheckout,
} from "./paddle.tsx";
import { BillingAccountPage } from "./pages/billing-account.tsx";
import { BillingAccountsPage } from "./pages/billing-accounts.tsx";
import { LogtoCallbackPage } from "./pages/callback.tsx";
import { HomePage } from "./pages/home.tsx";
import { InvitationPage } from "./pages/invitation.tsx";
import {
  Link,
  isInvitationFlowRoute,
  parseRoute,
  type Route,
  RouteId,
  routePath,
  RouterProvider,
  useRouter,
} from "./router.tsx";

/**
 * Deletes a pending invitation token when the user navigates inside the app
 * away from the invitation flow. The sign-in callback is part of the flow.
 */
export function clearInvitationOnLeave(_from: Route, to: Route): void {
  if (!isInvitationFlowRoute(to)) {
    pendingInvitation.clear();
  }
}

/**
 * Deletes a pending invitation token when the app starts outside the
 * invitation flow. Router navigation listeners never see the initial route,
 * so an abandoned sign-in redirect, a sign-out reload or any other fresh load
 * of a non-invitation route would otherwise keep the token for the tab's
 * lifetime.
 */
export function clearInvitationOnStart(route: Route): void {
  if (!isInvitationFlowRoute(route)) {
    pendingInvitation.clear();
  }
}

/** The whole app with the auth provider of its configuration. */
export function App({ config }: { config: AccountsConfig }) {
  const checkout = useMemo(
    () =>
      config.paddle === undefined
        ? undefined
        : createPaddleCheckout(config.paddle),
    [config.paddle],
  );
  const body = <Accounts config={config} checkout={checkout} />;
  if (config.logto === undefined) {
    return (
      <AuthContextProvider value={NOT_CONFIGURED}>{body}</AuthContextProvider>
    );
  }
  return <LogtoAuthProvider settings={config.logto}>{body}</LogtoAuthProvider>;
}

/**
 * The app below the auth provider: API transport, React Query, the Paddle
 * checkout, the router and the pages. Tests render it with their own auth,
 * transport and checkout.
 */
export function Accounts({
  config,
  transport,
  queryClient,
  checkout,
}: {
  config: AccountsConfig;
  transport?: Transport;
  queryClient?: QueryClient;
  checkout: Checkout | undefined;
}) {
  const client = useMemo(
    () => queryClient ?? createQueryClient(),
    [queryClient],
  );
  // Run once before the first render. The clear is idempotent, so a repeated
  // initializer call under StrictMode is harmless.
  useState(() => clearInvitationOnStart(parseRoute(window.location.pathname)));
  return (
    <ApiProviders config={config} transport={transport} queryClient={client}>
      <CheckoutProvider checkout={checkout}>
        <RouterProvider onNavigate={clearInvitationOnLeave}>
          <ToastRegion />
          <Routes config={config} />
        </RouterProvider>
      </CheckoutProvider>
    </ApiProviders>
  );
}

function ApiProviders({
  config,
  transport,
  queryClient,
  children,
}: {
  config: AccountsConfig;
  transport: Transport | undefined;
  queryClient: QueryClient;
  children: ReactNode;
}) {
  const auth = useAuth();
  const getToken = auth.getAccessToken;
  // Keep the transport stable while the token source changes identity.
  const [tokenRef] = useState(() => ({ current: getToken }));
  tokenRef.current = getToken;
  const getAccessToken = useCallback(() => tokenRef.current(), [tokenRef]);
  const resolved = useMemo(
    () => transport ?? createApiTransport(config.apiOrigin, getAccessToken),
    [transport, config.apiOrigin, getAccessToken],
  );
  return (
    <TransportProvider transport={resolved}>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </TransportProvider>
  );
}

function Routes({ config }: { config: AccountsConfig }) {
  const auth = useAuth();
  const { route } = useRouter();
  const configured = config.logto !== undefined;
  if (route.id === RouteId.Callback) {
    // Without a Logto registration there is no provider for the Logto
    // callback hook. Show the notice instead.
    return config.logto !== undefined ? (
      <LogtoCallbackPage appId={config.logto.appId} />
    ) : (
      <SignIn configured={false} />
    );
  }
  if (auth.isLoading) {
    return null;
  }
  switch (route.id) {
    case RouteId.Home:
      return <HomePage />;
    case RouteId.BillingInvitation:
      return <InvitationPage configured={configured} />;
    case RouteId.NotFound:
      return (
        <Page narrow>
          <PageHeader title="Page not found" />
          <Link to={routePath({ id: RouteId.Home })}>Go to the home page</Link>
        </Page>
      );
    default:
      break;
  }
  if (!auth.isAuthenticated) {
    return <SignIn configured={configured} />;
  }
  switch (route.id) {
    case RouteId.Billing:
      return <BillingAccountsPage />;
    case RouteId.BillingAccount:
      return (
        <BillingAccountPage
          billingAccountId={route.billingAccountId}
          key={route.billingAccountId}
        />
      );
  }
}

function SignIn({ configured }: { configured: boolean }) {
  const auth = useAuth();
  return (
    <Page narrow>
      <PageHeader
        title="Sign in to manage billing"
        description="Use your Delino account to manage billing accounts, credit and invoices."
      />
      {configured ? (
        <>
          <Button onClick={() => auth.signIn()}>Sign in with Delino</Button>
          <SignInFailureNotice />
        </>
      ) : (
        <p role="alert">Sign-in is not configured for this build.</p>
      )}
    </Page>
  );
}
