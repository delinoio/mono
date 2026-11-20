// SPDX-License-Identifier: Apache-2.0
import {
  LogtoProvider,
  useHandleSignInCallback,
  useLogto,
  type LogtoConfig,
} from "@logto/react";
import { useEffect, useMemo, useRef, useState } from "react";
import { API_RESOURCE, type LogtoSettings } from "../config.ts";
import { SIGN_IN_CALLBACK_PATH } from "../invitation.ts";
import {
  AccessTokenUnavailableError,
  AuthContextProvider,
  AuthStatus,
  type AuthApi,
  type AuthProviderProps,
} from "./auth.tsx";

function LogtoCallback({ onComplete }: { onComplete: () => void }) {
  const { isLoading, error } = useHandleSignInCallback(onComplete);
  if (error) {
    // Do not render the error message: it can contain request details.
    return <p role="alert">Sign-in failed. Go back and try again.</p>;
  }
  return isLoading ? <p>Signing in…</p> : null;
}

function LogtoAuthBridge({ children }: AuthProviderProps) {
  const logto = useLogto();
  const [userId, setUserId] = useState<string | undefined>(undefined);
  // `useLogto` returns new function identities on every render; keep the
  // latest in a ref so the API object stays stable for React Query.
  const logtoRef = useRef(logto);
  logtoRef.current = logto;

  useEffect(() => {
    if (!logto.isAuthenticated) {
      setUserId(undefined);
      return;
    }
    let cancelled = false;
    void logtoRef.current.getIdTokenClaims().then((claims) => {
      if (!cancelled) {
        setUserId(claims?.sub);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [logto.isAuthenticated]);

  const status = logto.isLoading
    ? AuthStatus.Loading
    : logto.isAuthenticated
      ? AuthStatus.SignedIn
      : AuthStatus.SignedOut;

  const value = useMemo<AuthApi>(
    () => ({
      status,
      userId,
      signIn: () =>
        logtoRef.current.signIn(
          `${window.location.origin}${SIGN_IN_CALLBACK_PATH}`,
        ),
      signOut: async () => {
        await logtoRef.current.signOut(`${window.location.origin}/`);
      },
      getAccessToken: async () => {
        const token = await logtoRef.current.getAccessToken(API_RESOURCE);
        if (!token) {
          throw new AccessTokenUnavailableError();
        }
        return token;
      },
      Callback: LogtoCallback,
    }),
    [status, userId],
  );
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}

/** Creates the auth provider for the console's Logto application. */
export function createLogtoAuthProvider(settings: LogtoSettings) {
  const config: LogtoConfig = {
    endpoint: settings.endpoint,
    appId: settings.appId,
    // The access token audience that ownsand-api validates.
    resources: [API_RESOURCE],
  };
  return function LogtoAuthProvider({ children }: AuthProviderProps) {
    return (
      <LogtoProvider config={config}>
        <LogtoAuthBridge>{children}</LogtoAuthBridge>
      </LogtoProvider>
    );
  };
}
