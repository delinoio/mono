// SPDX-License-Identifier: Apache-2.0
import { useEffect, useMemo, useState } from "react";
import { SIGN_IN_CALLBACK_PATH } from "../invitation.ts";
import {
  AccessTokenUnavailableError,
  AuthContextProvider,
  AuthStatus,
  type AuthApi,
  type AuthProviderProps,
} from "./auth.tsx";

/**
 * WORKAROUND: sign-in stand-in for the e2e suite only.
 *
 * Why: the e2e contract forbids a real Logto tenant in browser tests, and the
 * Logto SDK needs a real OpenID Connect provider for its redirect flow.
 * Scope: loaded only when the development server runs with
 * `PUBLIC_OWNSAND_E2E_FAKE_AUTH=1` (src/index.tsx checks `import.meta.env.DEV`
 * first, so production bundles drop this module, and rsbuild.config.ts fails
 * a production build that sets the variable).
 * Removal: replace it when the e2e framework can run a local OpenID Connect
 * provider, or when the suites gain a Logto test tenant.
 *
 * It mimics the redirect: `signIn` loads `/callback` as a new document, so the
 * URL fragment is lost exactly as in a Logto redirect. The signed-in flag is
 * kept in `sessionStorage` under `FAKE_SESSION_KEY`, standing in for the Logto
 * SDK session state; it holds no token.
 */
export const FAKE_SESSION_KEY = "ownsand.e2e.fakeSession";

/** User ID of the stand-in user. */
export const FAKE_USER_ID = "e2e-user";

/** Bearer value that the stand-in sends; the e2e suite mocks the API. */
const FAKE_ACCESS_TOKEN = "e2e-fake-access-token";

function readSignedIn(): boolean {
  try {
    return window.sessionStorage.getItem(FAKE_SESSION_KEY) === "1";
  } catch {
    return false;
  }
}

export function FakeAuthProvider({ children }: AuthProviderProps) {
  const [signedIn, setSignedIn] = useState(readSignedIn);

  // The callback route marks the session as signed in. It is created once
  // and closes over the state setter, so no event wiring is needed.
  const Callback = useMemo(
    () =>
      function FakeCallback({ onComplete }: { onComplete: () => void }) {
        useEffect(() => {
          window.sessionStorage.setItem(FAKE_SESSION_KEY, "1");
          setSignedIn(true);
          onComplete();
        }, [onComplete]);
        return <p>Signing in…</p>;
      },
    [],
  );

  const value = useMemo<AuthApi>(
    () => ({
      status: signedIn ? AuthStatus.SignedIn : AuthStatus.SignedOut,
      userId: signedIn ? FAKE_USER_ID : undefined,
      signIn: () => {
        window.location.assign(SIGN_IN_CALLBACK_PATH);
        return Promise.resolve();
      },
      signOut: () => {
        window.sessionStorage.removeItem(FAKE_SESSION_KEY);
        window.location.assign("/");
        return Promise.resolve();
      },
      getAccessToken: () =>
        readSignedIn()
          ? Promise.resolve(FAKE_ACCESS_TOKEN)
          : Promise.reject(new AccessTokenUnavailableError()),
      Callback,
    }),
    [signedIn, Callback],
  );
  return <AuthContextProvider value={value}>{children}</AuthContextProvider>;
}
