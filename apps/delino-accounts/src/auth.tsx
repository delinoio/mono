// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, PersistKey, useLogto } from "@logto/react";
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

/** What the stored Logto sign-in session says about the visit of the callback. */
export enum LogtoSignInRedirect {
  /** No sign-in redirect for this address: nothing for the SDK to complete. */
  None = "none",
  /** A readable session names this address: the SDK completes the redirect. */
  Pending = "pending",
  /** A stored session cannot be read: malformed JSON or an invalid shape. */
  Unreadable = "unreadable",
}

/**
 * Classifies the sign-in redirect that the Logto SDK has to complete at this
 * address.
 *
 * `useHandleSignInCallback` reports nothing when no redirect is pending: no
 * loading state, no error and no callback. Its `isLoading` is also `false`
 * for a render while a real redirect starts, so the hook state cannot tell
 * the two cases apart, and `@logto/react` exposes neither the client nor its
 * `isSignInRedirected` method. The app therefore applies the same rule as
 * `isSignInRedirected` (`@logto/client` 3.2.x) to the SDK's stored sign-in
 * session: a redirect is pending when `sessionStorage` holds the session of
 * the application (key `logto:<appId>:signInSession`, or the legacy key
 * `logto:<appId>`) and its `redirectUri` equals the origin and path of this
 * address.
 *
 * A stored session that is not valid JSON, or whose `redirectUri`,
 * `codeVerifier` or `state` is not a string, is `Unreadable`. The SDK throws
 * for it before the hook starts its error handling, so the rejection never
 * reaches the hook's `error` and the page would render nothing; the caller
 * must show the failure itself and must not mount the hook. Remove this check
 * when `@logto/react` exposes the client or the pending state, or reports
 * this failure through the hook.
 */
export function readLogtoSignInRedirect(
  appId: string,
  location: Pick<Location, "origin" | "pathname">,
  storage: Pick<Storage, "getItem"> | undefined = browserSessionStorage(),
): LogtoSignInRedirect {
  let stored: string | null;
  try {
    const key = `logto:${appId}`;
    stored =
      storage?.getItem(`${key}:${PersistKey.SignInSession}`) ??
      storage?.getItem(key) ??
      null;
  } catch {
    return LogtoSignInRedirect.None;
  }
  if (stored === null || stored === "") return LogtoSignInRedirect.None;
  let session: unknown;
  try {
    session = JSON.parse(stored);
  } catch {
    return LogtoSignInRedirect.Unreadable;
  }
  if (!isSignInSession(session)) return LogtoSignInRedirect.Unreadable;
  return session.redirectUri === `${location.origin}${location.pathname}`
    ? LogtoSignInRedirect.Pending
    : LogtoSignInRedirect.None;
}

/** The shape check of `isLogtoSignInSessionItem` in `@logto/client` 3.2.x. */
function isSignInSession(
  value: unknown,
): value is { redirectUri: string; codeVerifier: string; state: string } {
  if (typeof value !== "object" || value === null) return false;
  const { redirectUri, codeVerifier, state } = value as Record<string, unknown>;
  return (
    typeof redirectUri === "string" &&
    typeof codeVerifier === "string" &&
    typeof state === "string"
  );
}

function browserSessionStorage(): Storage | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

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
