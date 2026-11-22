// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { ToastRegion } from "delino-ui/toast";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  createApiTransport,
  createQueryClient,
  watchBillingErrors,
} from "./api.ts";
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
import { pendingInvitation } from "./invitation.ts";
import { ApiKeysPage } from "./pages/api-keys.tsx";
import { LogtoCallbackPage, MockCallbackPage } from "./pages/callback.tsx";
import { DevicePage, DevicesPage } from "./pages/devices.tsx";
import { EnrollmentTokensPage } from "./pages/enrollment-tokens.tsx";
import { InvitationPage } from "./pages/invitation.tsx";
import { MembersPage } from "./pages/members.tsx";
import { OrganizationsPage } from "./pages/organizations.tsx";
import { SandboxPage } from "./pages/sandbox.tsx";
import { SandboxesPage } from "./pages/sandboxes.tsx";
import { SettingsPage } from "./pages/settings.tsx";
import { SnapshotsPage } from "./pages/snapshots.tsx";
import {
  Link,
  type Route,
  RouteId,
  RouterProvider,
  useRouter,
} from "./router.tsx";

/**
 * Clears a pending invitation secret when the user navigates inside the
 * console away from the invitation flow. The sign-in callback is part of
 * the flow.
 */
export function clearInvitationOnLeave(_from: Route, to: Route): void {
  if (to.id !== RouteId.Invitation && to.id !== RouteId.Callback) {
    pendingInvitation.clear();
  }
}

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
      <RouterProvider onNavigate={clearInvitationOnLeave}>
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
  useEffect(() => watchBillingErrors(queryClient), [queryClient]);
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
    if (config.authMode === AuthMode.Mock) {
      return <MockCallbackPage />;
    }
    // Without a Logto registration there is no provider for the Logto
    // callback hook, which would render nothing. Show the notice instead.
    if (config.logto === undefined) {
      return <SignIn configured={false} />;
    }
    return <LogtoCallbackPage appId={config.logto.appId} />;
  }
  if (auth.isLoading) {
    return null;
  }
  if (route.id === RouteId.Invitation) {
    return <InvitationPage configured={configured} />;
  }
  if (!auth.isAuthenticated) {
    return <SignIn configured={configured} />;
  }
  switch (route.id) {
    case RouteId.Organizations:
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

function OrgPage({ route }: { route: Route }) {
  switch (route.id) {
    case RouteId.Sandboxes:
      return <SandboxesPage />;
    case RouteId.Sandbox:
      return <SandboxPage sandboxId={route.sandboxId} key={route.sandboxId} />;
    case RouteId.Devices:
      return <DevicesPage />;
    case RouteId.Device:
      return <DevicePage deviceId={route.deviceId} key={route.deviceId} />;
    case RouteId.Members:
      return <MembersPage />;
    case RouteId.EnrollmentTokens:
      return <EnrollmentTokensPage />;
    case RouteId.ApiKeys:
      return <ApiKeysPage />;
    case RouteId.Snapshots:
      return <SnapshotsPage />;
    case RouteId.Settings:
      return <SettingsPage />;
    default:
      return null;
  }
}

function SignIn({ configured }: { configured: boolean }) {
  const auth = useAuth();
  return (
    <main className="sign-in">
      <h1>OwnSand</h1>
      <p className="muted">
        Run sandboxes on the devices your team already owns.
      </p>
      {configured ? (
        <Button onClick={() => auth.signIn()}>Sign in with Delino</Button>
      ) : (
        <p role="alert">{SIGN_IN_NOT_CONFIGURED}</p>
      )}
    </main>
  );
}
