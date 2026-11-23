// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, PersistKey, useLogto } from "@logto/react";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
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

/** What the SDK's stored sign-in session says about this address. */
export enum SignInSessionState {
  /** No redirect is pending: there is no stored session, or it is for another address. */
  None = "none",
  /** A readable session names this address, so the SDK completes a redirect. */
  Pending = "pending",
  /** The stored session cannot be read, so the SDK hook would reject. */
  Invalid = "invalid",
}

/**
 * The state of the SDK's stored sign-in session at this address.
 *
 * `useHandleSignInCallback` reports nothing when no redirect is pending: no
 * loading state, no error and no callback. Its `isLoading` is also `false`
 * for a render while a real redirect starts, so the hook state cannot tell
 * the two cases apart, and `@logto/react` exposes neither the client nor its
 * `isSignInRedirected` method. The console therefore applies the same rules
 * as `getSignInSession` and `isSignInRedirected` (`@logto/client` 3.2.x) to
 * the SDK's stored sign-in session: `sessionStorage` holds the session of
 * the application under the key `logto:<appId>:signInSession`, or under the
 * legacy key `logto:<appId>`. A missing or empty value is `None`. A value that
 * is not JSON, or not an object whose `redirectUri`, `codeVerifier` and
 * `state` are all strings, is `Invalid`: the client throws on it, and the hook
 * of `@logto/react` awaits `isSignInRedirected` outside its error handling, so
 * the failure would be an unhandled rejection and the page would stay blank
 * instead of showing the sign-in error. A valid session is `Pending` when its
 * `redirectUri` equals the origin and path of this address, and `None`
 * otherwise. A storage that throws counts as `None`. Remove this check when
 * `@logto/react` exposes the client or the pending state, or handles an
 * invalid session itself.
 */
export function logtoSignInSessionState(
  appId: string,
  location: Pick<Location, "origin" | "pathname">,
  storage: Pick<Storage, "getItem"> | undefined = browserSessionStorage(),
): SignInSessionState {
  let stored: string | null;
  try {
    const key = `logto:${appId}`;
    stored =
      storage?.getItem(`${key}:${PersistKey.SignInSession}`) ??
      storage?.getItem(key) ??
      null;
  } catch {
    return SignInSessionState.None;
  }
  if (stored === null || stored === "") return SignInSessionState.None;
  let session: unknown;
  try {
    session = JSON.parse(stored);
  } catch {
    return SignInSessionState.Invalid;
  }
  if (typeof session !== "object" || session === null) {
    return SignInSessionState.Invalid;
  }
  const { redirectUri, codeVerifier, state } = session as Record<
    string,
    unknown
  >;
  if (
    typeof redirectUri !== "string" ||
    typeof codeVerifier !== "string" ||
    typeof state !== "string"
  ) {
    return SignInSessionState.Invalid;
  }
  return redirectUri === `${location.origin}${location.pathname}`
    ? SignInSessionState.Pending
    : SignInSessionState.None;
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
 * One `signIn` call. `baseline` is the SDK error before the call, so that an
 * earlier error never counts as this attempt's failure. `failed` stays
 * `undefined` until the SDK call resolved and the SDK error state of that
 * render was compared with the baseline.
 */
interface SignInAttempt {
  baseline: unknown;
  settled: boolean;
  failed: boolean | undefined;
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
    error: sdkError,
    signIn,
    signOut,
  } = logto;
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
      signIn: async () => {
        const id = ++attemptCount.current;
        const baseline = sdkErrorRef.current;
        setAttempt({ baseline, settled: false, failed: undefined });
        await signIn(window.location.origin + CALLBACK_PATH);
        // A newer attempt replaced this one while it ran.
        if (id === attemptCount.current) {
          setAttempt({ baseline, settled: true, failed: undefined });
        }
      },
      signInFailed: attempt?.failed === true,
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
      attempt?.failed,
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
      signInFailed: false,
      signOut,
      getAccessToken: async () => (signedIn ? "mock-access-token" : undefined),
    }),
    [signedIn, signIn, signOut],
  );
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}
