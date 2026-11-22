// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, useLogto } from "@logto/react";
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
