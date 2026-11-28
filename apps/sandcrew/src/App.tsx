// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastRegion } from "delino-ui/toast";
import { type ReactNode, useCallback, useMemo, useState } from "react";
import { createApiTransport, createQueryClient } from "./api.ts";
import {
  type Auth,
  AuthContextProvider,
  LogtoAuthProvider,
  MockAuthProvider,
  SIGN_IN_NOT_CONFIGURED,
  useAuth,
} from "./auth.tsx";
import { OrgShell, TopBar } from "./components/shell.tsx";
import { PageHeader, SignInAction } from "./components/ui.tsx";
import { AuthMode, type ConsoleConfig } from "./config.ts";
import { LogtoCallbackPage, MockCallbackPage } from "./pages/callback.tsx";
import { OrganizationsPage } from "./pages/organizations.tsx";
import { SessionChatPage } from "./pages/session-chat.tsx";
import { SessionListPage } from "./pages/session-list.tsx";
import { SessionNewPage } from "./pages/session-new.tsx";
import { OrganizationSettingsPage } from "./pages/settings-organization.tsx";
import { UserSettingsPage } from "./pages/settings-user.tsx";
import {
  Link,
  type Route,
  RouteId,
  RouterProvider,
  useRouter,
} from "./router.tsx";

/** The whole console with the auth provider of its configuration. */
export function App({
  config,
  mockTransport,
}: {
  config: ConsoleConfig;
  /** The mock API transport; only a mock build passes it. */
  mockTransport?: Transport;
}) {
  const body = <Console config={config} transport={mockTransport} />;
  if (config.authMode === AuthMode.Mock) {
    return <MockAuthProvider>{body}</MockAuthProvider>;
  }
  if (config.logto === undefined) {
    return (
      <AuthContextProvider value={NOT_CONFIGURED}>{body}</AuthContextProvider>
    );
  }
  return <LogtoAuthProvider settings={config.logto}>{body}</LogtoAuthProvider>;
}

/**
 * The console below the auth provider: API transport, React Query, router
 * and pages. Tests render it with their own auth and transport.
 */
export function Console({
  config,
  transport,
  queryClient,
}: {
  config: ConsoleConfig;
  transport?: Transport;
  queryClient?: QueryClient;
}) {
  const client = useMemo(
    () => queryClient ?? createQueryClient(),
    [queryClient],
  );
  return (
    <ApiProviders config={config} transport={transport} queryClient={client}>
      <RouterProvider>
        <ToastRegion />
        <Routes config={config} />
      </RouterProvider>
    </ApiProviders>
  );
}

const NOT_CONFIGURED: Auth = {
  isLoading: false,
  isAuthenticated: false,
  userId: undefined,
  signIn: async () => {},
  signInFailed: false,
  signOut: async () => {},
  getAccessToken: async () => undefined,
};

function ApiProviders({
  config,
  transport,
  queryClient,
  children,
}: {
  config: ConsoleConfig;
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

function Routes({ config }: { config: ConsoleConfig }) {
  const auth = useAuth();
  const { route } = useRouter();
  const configured =
    config.authMode === AuthMode.Mock || config.logto !== undefined;
  if (route.id === RouteId.SignInCallback) {
    if (config.authMode === AuthMode.Mock) return <MockCallbackPage />;
    if (config.logto !== undefined) {
      return <LogtoCallbackPage appId={config.logto.appId} />;
    }
    return <SignIn configured={false} />;
  }
  if (auth.isLoading) {
    return null;
  }
  if (!auth.isAuthenticated) {
    return <SignIn configured={configured} />;
  }
  switch (route.id) {
    case RouteId.OrganizationPick:
      return <OrganizationsPage />;
    case RouteId.NotFound:
      return (
        <>
          <TopBar />
          <main className="content content-narrow">
            <PageHeader title="Page not found" />
            <Link to="/">Go to your organizations</Link>
          </main>
        </>
      );
    default:
      return (
        <OrgShell
          organizationId={route.organizationId}
          key={route.organizationId}
        >
          <OrgPage route={route} />
        </OrgShell>
      );
  }
}

function OrgPage({
  route,
}: {
  route: Extract<Route, { organizationId: string }>;
}) {
  switch (route.id) {
    case RouteId.SessionList:
      return <SessionListPage />;
    case RouteId.SessionNew:
      return <SessionNewPage />;
    case RouteId.SessionChat:
      return (
        <SessionChatPage key={route.sessionId} sessionId={route.sessionId} />
      );
    case RouteId.SettingsUser:
      return <UserSettingsPage />;
    case RouteId.SettingsOrg:
      return <OrganizationSettingsPage />;
  }
}

function SignIn({ configured }: { configured: boolean }) {
  return (
    <main className="sign-in">
      <h1>SandCrew</h1>
      <p className="muted">
        Start coding-agent sessions in your SandRun sandboxes and talk to the
        agent in a chat.
      </p>
      {configured ? (
        <SignInAction />
      ) : (
        <p role="alert">{SIGN_IN_NOT_CONFIGURED}</p>
      )}
    </main>
  );
}
