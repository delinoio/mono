// SPDX-License-Identifier: Apache-2.0
import {
  createContext,
  useContext,
  type ComponentType,
  type ReactNode,
} from "react";

/** Sign-in state of the console user. */
export enum AuthStatus {
  Loading = "loading",
  SignedOut = "signed-out",
  SignedIn = "signed-in",
  /** The build has no Logto settings, so nobody can sign in. */
  Unconfigured = "unconfigured",
}

/**
 * The console's view of authentication. The production implementation wraps
 * the Logto React SDK (src/auth/logto-auth.tsx). The e2e development server
 * replaces it with a stand-in (src/auth/fake-auth.tsx) that never reaches a
 * production bundle.
 */
export interface AuthApi {
  readonly status: AuthStatus;
  /** Redirects to sign-in. The sign-in callback returns to `/callback`. */
  readonly signIn: () => Promise<void>;
  /** Signs out and returns to the console root. */
  readonly signOut: () => Promise<void>;
  /**
   * Returns an access token for the ownsand-api resource. Rejects when no
   * token is available. Callers must never log the value.
   */
  readonly getAccessToken: () => Promise<string>;
  /** The Logto user ID (`sub`) of the signed-in user. */
  readonly userId: string | undefined;
  /** Renders the sign-in callback route and calls `onComplete` when done. */
  readonly Callback: ComponentType<{ onComplete: () => void }>;
}

/** Error thrown when an access token is not available. */
export class AccessTokenUnavailableError extends Error {
  constructor() {
    super("No access token is available. Sign in again.");
    this.name = "AccessTokenUnavailableError";
  }
}

const AuthContext = createContext<AuthApi | undefined>(undefined);

export function AuthContextProvider({
  value,
  children,
}: {
  readonly value: AuthApi;
  readonly children: ReactNode;
}) {
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthApi {
  const value = useContext(AuthContext);
  if (value === undefined) {
    throw new Error("useAuth must be used inside an auth provider");
  }
  return value;
}

/** Props of every auth provider component. */
export interface AuthProviderProps {
  readonly children: ReactNode;
}

function UnconfiguredCallback() {
  return null;
}

/** Auth provider for a build without Logto settings. */
export function UnconfiguredAuthProvider({ children }: AuthProviderProps) {
  const value: AuthApi = {
    status: AuthStatus.Unconfigured,
    signIn: () => Promise.resolve(),
    signOut: () => Promise.resolve(),
    getAccessToken: () => Promise.reject(new AccessTokenUnavailableError()),
    userId: undefined,
    Callback: UnconfiguredCallback,
  };
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}
