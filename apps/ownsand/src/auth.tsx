// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, PersistKey, useLogto } from "@logto/react";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { API_RESOURCE, type LogtoSettings } from "./config.ts";
import { pendingInvitation } from "./invitation.ts";

/** The signed-in state that the console needs, independent of the provider. */
export interface Auth {
  isLoading: boolean;
  isAuthenticated: boolean;
  /** The Logto user ID (`sub`) of the signed-in user. */
  userId: string | undefined;
  signIn(): Promise<void>;
  signOut(): Promise<void>;
  /** A current access token for the control plane API resource. */
  getAccessToken(): Promise<string | undefined>;
}

const AuthContext = createContext<Auth | undefined>(undefined);

export function useAuth(): Auth {
  const auth = useContext(AuthContext);
  if (auth === undefined) {
    throw new Error("useAuth needs an auth provider");
  }
  return auth;
}

export function AuthContextProvider({
  value,
  children,
}: {
  value: Auth;
  children: ReactNode;
}) {
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

/** The notice of a build that has no sign-in configuration. */
export const SIGN_IN_NOT_CONFIGURED =
  "Sign-in is not configured for this build.";

/** Path of the sign-in callback route. */
export const CALLBACK_PATH = "/callback";

/**
 * Whether the Logto SDK has a sign-in redirect to complete at this address.
 *
 * `useHandleSignInCallback` reports nothing when no redirect is pending: no
 * loading state, no error and no callback. Its `isLoading` is also `false`
 * for a render while a real redirect starts, so the hook state cannot tell
 * the two cases apart, and `@logto/react` exposes neither the client nor its
 * `isSignInRedirected` method. The console therefore applies the same rule
 * as `isSignInRedirected` (`@logto/client` 3.2.x) to the SDK's stored
 * sign-in session: a redirect is pending when `sessionStorage` holds the
 * session of the application (key `logto:<appId>:signInSession`, or the
 * legacy key `logto:<appId>`) and, when the session is readable, its
 * `redirectUri` equals the origin and path of this address. An unreadable
 * session counts as pending, because the hook then reports an error that the
 * page shows. Remove this check when `@logto/react` exposes the client or
 * the pending state.
 */
export function isLogtoSignInPending(
  appId: string,
  location: Pick<Location, "origin" | "pathname">,
  storage: Pick<Storage, "getItem"> | undefined = browserSessionStorage(),
): boolean {
  let stored: string | null;
  try {
    const key = `logto:${appId}`;
    stored =
      storage?.getItem(`${key}:${PersistKey.SignInSession}`) ??
      storage?.getItem(key) ??
      null;
  } catch {
    return false;
  }
  if (stored === null || stored === "") return false;
  try {
    const session: unknown = JSON.parse(stored);
    const redirectUri =
      typeof session === "object" && session !== null
        ? (session as { redirectUri?: unknown }).redirectUri
        : undefined;
    if (typeof redirectUri !== "string") return true;
    return redirectUri === `${location.origin}${location.pathname}`;
  } catch {
    return true;
  }
}

function browserSessionStorage(): Storage | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

/**
 * Logto single-page application sign-in. The SDK requests access tokens for
 * the control plane API resource; the console never stores tokens itself.
 */
export function LogtoAuthProvider({
  settings,
  children,
}: {
  settings: LogtoSettings;
  children: ReactNode;
}) {
  const config = useMemo(
    () => ({
      endpoint: settings.endpoint,
      appId: settings.appId,
      resources: [API_RESOURCE],
      scopes: settings.scopes,
    }),
    [settings],
  );
  return (
    <LogtoProvider config={config}>
      <LogtoBridge>{children}</LogtoBridge>
    </LogtoProvider>
  );
}

/**
 * Adapts the Logto SDK state to `Auth`. Exported for tests, which render it
 * inside a `LogtoProvider` with a fake client.
 */
export function LogtoBridge({ children }: { children: ReactNode }) {
  const logto = useLogto();
  const [userId, setUserId] = useState<string | undefined>();
  const [initialized, setInitialized] = useState(false);
  const {
    isAuthenticated,
    isLoading: sdkLoading,
    getIdTokenClaims,
    getAccessToken,
    signIn,
    signOut,
  } = logto;
  // The SDK sets `isLoading` for every token request, not only while it
  // restores the session. The console unmounts its pages while `isLoading`
  // is true, so each page that requested a token unmounted itself, mounted
  // again and requested the token again without end. Report loading only
  // until the SDK has restored the session for the first time.
  useEffect(() => {
    if (!sdkLoading) {
      setInitialized(true);
    }
  }, [sdkLoading]);
  useEffect(() => {
    if (!isAuthenticated) {
      setUserId(undefined);
      return;
    }
    void getIdTokenClaims().then((claims) => setUserId(claims?.sub));
  }, [isAuthenticated, getIdTokenClaims]);
  const value: Auth = useMemo(
    () => ({
      isLoading: sdkLoading && !initialized,
      isAuthenticated,
      userId,
      signIn: () => signIn(window.location.origin + CALLBACK_PATH),
      signOut: async () => {
        // A pending invitation secret must not outlive the session.
        pendingInvitation.clear();
        await signOut(window.location.origin);
      },
      getAccessToken: async () =>
        (await getAccessToken(API_RESOURCE)) ?? undefined,
    }),
    [
      sdkLoading,
      initialized,
      isAuthenticated,
      userId,
      signIn,
      signOut,
      getAccessToken,
    ],
  );
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}

/** The fake user of the mock API. */
export const MOCK_USER_ID = "mock-user-owner";

/**
 * The sign-in of the mock API (development server and end-to-end suite
 * only). Sign-in behaves like the Logto redirect: it moves to the callback
 * route with a full page load, which keeps `sessionStorage`. The fake
 * session itself lives in `sessionStorage`, like a provider session.
 */
export function MockAuthProvider({ children }: { children: ReactNode }) {
  const key = "ownsand.mock-session";
  const [signedIn, setSignedIn] = useState(() => {
    try {
      return window.sessionStorage.getItem(key) === "signed-in";
    } catch {
      return false;
    }
  });
  const signIn = useCallback(async () => {
    try {
      window.sessionStorage.setItem(key, "signed-in");
    } catch {
      setSignedIn(true);
      return;
    }
    window.location.assign(CALLBACK_PATH);
  }, []);
  const signOut = useCallback(async () => {
    pendingInvitation.clear();
    try {
      window.sessionStorage.removeItem(key);
    } catch {
      // Nothing stored.
    }
    setSignedIn(false);
    window.location.assign("/");
  }, []);
  const value: Auth = useMemo(
    () => ({
      isLoading: false,
      isAuthenticated: signedIn,
      userId: signedIn ? MOCK_USER_ID : undefined,
      signIn,
      signOut,
      getAccessToken: async () => (signedIn ? "mock-access-token" : undefined),
    }),
    [signedIn, signIn, signOut],
  );
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}
