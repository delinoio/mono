// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, useLogto } from "@logto/react";
import {
  createContext,
  type ReactNode,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { API_RESOURCE, type LogtoSettings } from "./config.ts";

/** The signed-in state that the pages need, independent of the provider. */
export interface Auth {
  isLoading: boolean;
  isAuthenticated: boolean;
  /** The Logto user ID (`sub`) of the signed-in user. */
  userId: string | undefined;
  /** Starts the sign-in redirect. */
  signIn(): Promise<void>;
  signOut(): Promise<void>;
  /** A current access token for the accounts-api resource. */
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

/** Path of the sign-in callback route. */
export const CALLBACK_PATH = "/callback";

/** The auth state of a build without a Logto registration. */
export const NOT_CONFIGURED: Auth = {
  isLoading: false,
  isAuthenticated: false,
  userId: undefined,
  signIn: async () => {},
  signOut: async () => {},
  getAccessToken: async () => undefined,
};

/**
 * Logto single-page application sign-in. The SDK requests access tokens for
 * the accounts-api resource with the scope `accounts:billing`; the app never
 * stores tokens itself.
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

/** Adapts the Logto SDK state to `Auth`. */
function LogtoBridge({ children }: { children: ReactNode }) {
  const {
    isAuthenticated,
    isLoading: sdkLoading,
    getIdTokenClaims,
    getAccessToken,
    signIn,
    signOut,
  } = useLogto();
  const [userId, setUserId] = useState<string | undefined>();
  const [initialized, setInitialized] = useState(false);
  // The SDK sets `isLoading` for every token request, not only while it
  // restores the session. The pages unmount while `isLoading` is true, so
  // report loading only until the SDK restored the session for the first
  // time; otherwise each token request would remount the page that made it.
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
