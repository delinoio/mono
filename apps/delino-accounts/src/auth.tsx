// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, PersistKey, useLogto } from "@logto/react";
import {
  createContext,
  type ReactNode,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { API_RESOURCE, type LogtoSettings } from "./config.ts";

/** The signed-in state that the pages need, independent of the provider. */
export interface Auth {
  isLoading: boolean;
  isAuthenticated: boolean;
  /** The Logto user ID (`sub`) of the signed-in user. */
  userId: string | undefined;
  /**
   * Starts the sign-in redirect. It never rejects: the Logto SDK reports a
   * failure before the redirect (for example an unreachable tenant) through
   * `signInFailed`.
   */
  signIn(): Promise<void>;
  /**
   * Whether the latest `signIn` call failed before the redirect started. The
   * next `signIn` call clears it, and an error of another SDK request (a
   * token request or the callback) never sets it.
   */
  signInFailed: boolean;
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

/**
 * Whether the Logto SDK has a sign-in redirect to complete at this address.
 *
 * `useHandleSignInCallback` reports nothing when no redirect is pending: no
 * loading state, no error and no callback. Its `isLoading` is also `false`
 * for a render while a real redirect starts, so the hook state cannot tell
 * the two cases apart, and `@logto/react` exposes neither the client nor its
 * `isSignInRedirected` method. The app therefore applies the same rule as
 * `isSignInRedirected` (`@logto/client` 3.2.x) to the SDK's stored sign-in
 * session: a redirect is pending when `sessionStorage` holds the session of
 * the application (key `logto:<appId>:signInSession`, or the legacy key
 * `logto:<appId>`) and, when the session is readable, its `redirectUri`
 * equals the origin and path of this address. An unreadable session counts as
 * pending, because the hook then reports an error that the page shows. Remove
 * this check when `@logto/react` exposes the client or the pending state.
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

/** The auth state of a build without a Logto registration. */
export const NOT_CONFIGURED: Auth = {
  isLoading: false,
  isAuthenticated: false,
  userId: undefined,
  signIn: async () => {},
  signInFailed: false,
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

/**
 * One `signIn` call. `baseline` is the SDK error before the call, so that an
 * earlier error never counts as this attempt's failure. `failed` stays
 * `undefined` until the SDK call resolved and the SDK error state of that
 * render was compared with the baseline.
 */
interface SignInAttempt {
  id: number;
  baseline: unknown;
  settled: boolean;
  failed: boolean | undefined;
}

/** Adapts the Logto SDK state to `Auth`. */
function LogtoBridge({ children }: { children: ReactNode }) {
  const {
    isAuthenticated,
    isLoading: sdkLoading,
    getIdTokenClaims,
    getAccessToken,
    error: sdkError,
    signIn,
    signOut,
  } = useLogto();
  // The SDK `signIn` never rejects. It stores a failure in the provider's
  // `error` state, which the SDK never clears, and resolves. The bridge
  // therefore compares that state with its value at the start of the attempt:
  // a new error object that appears while the attempt runs is the failure of
  // that attempt, and a token or callback error after the attempt settled is
  // not. Each failure creates a new error object.
  const sdkErrorRef = useRef(sdkError);
  sdkErrorRef.current = sdkError;
  const attemptCount = useRef(0);
  const [attempt, setAttempt] = useState<SignInAttempt | undefined>();
  useEffect(() => {
    // The SDK sets its error before the promise of `signIn` resolves, so the
    // render that sees `settled` also sees the error of the attempt.
    if (attempt?.settled && attempt.failed === undefined) {
      setAttempt({
        ...attempt,
        failed: sdkError !== undefined && sdkError !== attempt.baseline,
      });
    }
  }, [attempt, sdkError]);
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
      signIn: async () => {
        const id = ++attemptCount.current;
        const baseline = sdkErrorRef.current;
        setAttempt({ id, baseline, settled: false, failed: undefined });
        await signIn(window.location.origin + CALLBACK_PATH);
        // A newer attempt replaced this one while it ran.
        if (id === attemptCount.current) {
          setAttempt({ id, baseline, settled: true, failed: undefined });
        }
      },
      signInFailed: attempt?.failed === true,
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
      attempt?.failed,
      signIn,
      signOut,
      getAccessToken,
    ],
  );
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}
