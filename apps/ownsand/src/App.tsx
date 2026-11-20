// SPDX-License-Identifier: Apache-2.0
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { Card, CardActions, CardDescription, CardTitle } from "delino-ui/card";
import {
  useCallback,
  useMemo,
  useRef,
  useState,
  type ComponentType,
  type ReactNode,
} from "react";
import { createApiTransport } from "./api/transport.ts";
import { AuthStatus, useAuth, type AuthProviderProps } from "./auth/auth.tsx";
import { LoadingState } from "./components/common.tsx";
import { ACCOUNT_SETTINGS_URL } from "./config.ts";
import {
  INVITATION_PATH,
  browserSessionStorage,
  clearPendingSecretOutsideFlow,
  readPendingSecret,
} from "./invitation.ts";
import { ApiKeysPage } from "./pages/api-keys-page.tsx";
import { DevicePage } from "./pages/device-page.tsx";
import { DevicesPage } from "./pages/devices-page.tsx";
import { EnrollmentTokensPage } from "./pages/enrollment-tokens-page.tsx";
import { InvitationPage } from "./pages/invitation-page.tsx";
import { MembersPage } from "./pages/members-page.tsx";
import { OrganizationLayout } from "./pages/organization-layout.tsx";
import { OrganizationsPage } from "./pages/organizations-page.tsx";
import { SandboxesPage } from "./pages/sandboxes-page.tsx";
import { SettingsPage } from "./pages/settings-page.tsx";
import { Link, RouterProvider, useRouter } from "./router.tsx";
import { RouteId, matchRoute } from "./routes.ts";

function Header() {
  const auth = useAuth();
  return (
    <header className="app-header">
      <Link className="brand" to="/">
        OwnSand
      </Link>
      <nav aria-label="Account" className="account-nav">
        <a className="text-link" href={ACCOUNT_SETTINGS_URL}>
          Account settings
        </a>
        {auth.status === AuthStatus.SignedIn ? (
          <Button variant={ButtonVariant.Secondary} onClick={auth.signOut}>
            Sign out
          </Button>
        ) : null}
      </nav>
    </header>
  );
}

function SignInView() {
  const auth = useAuth();
  if (auth.status === AuthStatus.Unconfigured) {
    return (
      <main className="page narrow">
        <Card>
          <CardTitle>Sign-in is not configured</CardTitle>
          <CardDescription>
            This build of the console has no Logto settings.
          </CardDescription>
        </Card>
      </main>
    );
  }
  return (
    <main className="page narrow">
      <Card>
        <CardTitle>Sign in to OwnSand</CardTitle>
        <CardDescription>
          Manage your organizations, devices and sandboxes with your Delino
          account.
        </CardDescription>
        <CardActions>
          <Button onClick={auth.signIn} autoFocus>
            Sign in
          </Button>
        </CardActions>
      </Card>
    </main>
  );
}

function NotFoundView() {
  return (
    <main className="page narrow">
      <h1>Page not found</h1>
      <Link className="text-link" to="/">
        Go to your organizations
      </Link>
    </main>
  );
}

function Routes() {
  const auth = useAuth();
  const { pathname, navigate } = useRouter();
  const match = matchRoute(pathname);

  const completeSignIn = useCallback(() => {
    const storage = browserSessionStorage();
    const pending = storage ? readPendingSecret(storage) : undefined;
    // Return to the invitation flow when a secret waits for this sign-in.
    navigate(pending !== undefined ? INVITATION_PATH : "/", { replace: true });
  }, [navigate]);

  if (match.id === RouteId.SignInCallback) {
    const Callback = auth.Callback;
    return (
      <main className="page narrow">
        <Callback onComplete={completeSignIn} />
      </main>
    );
  }
  if (match.id === RouteId.Invitation) {
    return <InvitationPage />;
  }
  if (auth.status === AuthStatus.Loading) {
    return (
      <main className="page narrow">
        <LoadingState />
      </main>
    );
  }
  if (auth.status !== AuthStatus.SignedIn) {
    return <SignInView />;
  }
  switch (match.id) {
    case RouteId.Organizations:
      return <OrganizationsPage />;
    case RouteId.NotFound:
      return <NotFoundView />;
    default:
      return (
        <OrganizationLayout
          key={match.organizationId}
          organizationId={match.organizationId}
          routeId={match.id}
          render={(props) => {
            switch (match.id) {
              case RouteId.Sandboxes:
                return <SandboxesPage {...props} />;
              case RouteId.Devices:
                return <DevicesPage {...props} />;
              case RouteId.Device:
                return (
                  <DevicePage {...props} deviceId={match.deviceId ?? ""} />
                );
              case RouteId.Members:
                return <MembersPage {...props} />;
              case RouteId.EnrollmentTokens:
                return <EnrollmentTokensPage {...props} />;
              case RouteId.ApiKeys:
                return <ApiKeysPage {...props} />;
              case RouteId.Settings:
                return <SettingsPage {...props} />;
            }
          }}
        />
      );
  }
}

function ApiProviders({
  apiOrigin,
  children,
}: {
  readonly apiOrigin: string;
  readonly children: ReactNode;
}) {
  const auth = useAuth();
  // The transport reads the latest token getter through a ref, so it is
  // created once and React Query caches stay valid across renders.
  const getAccessToken = useRef(auth.getAccessToken);
  getAccessToken.current = auth.getAccessToken;
  const transport = useMemo(
    () => createApiTransport(apiOrigin, () => getAccessToken.current()),
    [apiOrigin],
  );
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: { retry: 1, refetchOnWindowFocus: false },
        },
      }),
  );
  return (
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </TransportProvider>
  );
}

export interface AppProps {
  readonly apiOrigin: string;
  readonly AuthProvider: ComponentType<AuthProviderProps>;
}

/** Clears a pending invitation secret when the user leaves the invitation flow. */
function clearPendingInvitation(pathname: string) {
  const storage = browserSessionStorage();
  if (storage) {
    clearPendingSecretOutsideFlow(storage, pathname);
  }
}

export function App({ apiOrigin, AuthProvider }: AppProps) {
  return (
    <RouterProvider onPathChange={clearPendingInvitation}>
      <AuthProvider>
        <ApiProviders apiOrigin={apiOrigin}>
          <div className="app">
            <Header />
            <Routes />
          </div>
        </ApiProviders>
      </AuthProvider>
    </RouterProvider>
  );
}
