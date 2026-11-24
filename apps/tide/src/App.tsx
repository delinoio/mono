// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
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
import { PageHeader } from "./components/ui.tsx";
import { AuthMode, type ConsoleConfig } from "./config.ts";
import { ApiKeysPage } from "./pages/api-keys.tsx";
import { AuditPage } from "./pages/audit.tsx";
import { LogtoCallbackPage, MockCallbackPage } from "./pages/callback.tsx";
import { DevSessionsPage } from "./pages/dev-sessions.tsx";
import { GitHubSetupPage } from "./pages/github-setup.tsx";
import { IntegrationsPage } from "./pages/integrations.tsx";
import { InvitationPage } from "./pages/invitation.tsx";
import { MembersPage } from "./pages/members.tsx";
import { OrganizationsPage } from "./pages/organizations.tsx";
import { OverviewPage } from "./pages/overview.tsx";
import { ProfilesPage } from "./pages/profiles.tsx";
import { RequestsPage } from "./pages/requests.tsx";
import { RunnersPage } from "./pages/runners.tsx";
import { SettingsPage } from "./pages/settings.tsx";
import {
  Link,
  type OrgRouteId,
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
  if (route.id === RouteId.Callback) {
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
    return (
      <SignIn
        configured={configured}
        notice={
          route.id === RouteId.Invitation
            ? "Sign in, then open the invitation link again."
            : route.id === RouteId.GitHubSetup
              ? "Sign in, then start the GitHub installation again from the integrations of your organization."
              : undefined
        }
      />
    );
  }
  switch (route.id) {
    case RouteId.Organizations:
      return <OrganizationsPage />;
    case RouteId.Invitation:
      return <InvitationPage />;
    case RouteId.GitHubSetup:
      return <GitHubSetupPage />;
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
          <OrgPage id={route.id} />
        </OrgShell>
      );
  }
}

function OrgPage({ id }: { id: OrgRouteId }) {
  switch (id) {
    case RouteId.Overview:
      return <OverviewPage />;
    case RouteId.Requests:
      return <RequestsPage />;
    case RouteId.Runners:
      return <RunnersPage />;
    case RouteId.Profiles:
      return <ProfilesPage />;
    case RouteId.Integrations:
      return <IntegrationsPage />;
    case RouteId.Members:
      return <MembersPage />;
    case RouteId.DevSessions:
      return <DevSessionsPage />;
    case RouteId.ApiKeys:
      return <ApiKeysPage />;
    case RouteId.Audit:
      return <AuditPage />;
    case RouteId.Settings:
      return <SettingsPage />;
  }
}

function SignIn({
  configured,
  notice,
}: {
  configured: boolean;
  notice?: string;
}) {
  const auth = useAuth();
  return (
    <main className="sign-in">
      <h1>tide</h1>
      <p className="muted">
        Run GitHub Actions jobs, coding-agent sessions and dev sessions on the
        devices your team already owns.
      </p>
      {notice !== undefined && configured ? <p>{notice}</p> : null}
      {configured ? (
        <>
          <Button onClick={() => auth.signIn()}>Sign in with Delino</Button>
          {auth.signInFailed ? (
            <p role="alert">
              Sign-in did not start. Check your connection and try again.
            </p>
          ) : null}
        </>
      ) : (
        <p role="alert">{SIGN_IN_NOT_CONFIGURED}</p>
      )}
    </main>
  );
}
