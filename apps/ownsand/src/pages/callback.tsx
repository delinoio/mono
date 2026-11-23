// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect, useRef, useState } from "react";
import { logtoSignInSessionState, SignInSessionState } from "../auth.tsx";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { pendingInvitation } from "../invitation.ts";
import { RouteId, routePath, useRouter } from "../router.tsx";

/** Where to go after sign-in: back to a pending invitation, or home. */
export function afterSignInPath(): string {
  return pendingInvitation.read() !== undefined
    ? routePath({ id: RouteId.Invitation })
    : routePath({ id: RouteId.Organizations });
}

/**
 * Shown for a stored sign-in session that the SDK cannot read. A fixed error
 * keeps the notice text independent of the SDK's parse error.
 */
const UNREADABLE_SESSION = new Error(
  "The saved sign-in session is not readable",
);

/**
 * Route `callback` with Logto: completes the sign-in redirect. A visit with
 * no sign-in redirect in progress (a typed address, a bookmark or a reload
 * after the SDK finished the sign-in) leaves the route for the same target.
 * The error of a failed callback drops the pending invitation secret. A
 * stored sign-in session that the SDK cannot read shows the same notice and
 * drops the secret too, without mounting the SDK hook, which would reject
 * outside its error handling for such a session.
 */
export function LogtoCallbackPage({ appId }: { appId: string }) {
  // Decide once on mount, before the hook completes a redirect and the SDK
  // removes its stored sign-in session.
  const [state] = useState(() =>
    logtoSignInSessionState(appId, window.location),
  );
  if (state === SignInSessionState.Invalid) {
    return <UnreadableSessionNotice />;
  }
  return <CompleteSignIn pending={state === SignInSessionState.Pending} />;
}

function UnreadableSessionNotice() {
  useEffect(() => {
    pendingInvitation.clear();
  }, []);
  return <ErrorNotice error={UNREADABLE_SESSION} />;
}

function CompleteSignIn({ pending }: { pending: boolean }) {
  const { navigate } = useRouter();
  const { isLoading, error } = useHandleSignInCallback(() => {
    navigate(afterSignInPath(), { replace: true });
  });
  // StrictMode runs effects twice and keeps refs between the runs.
  const left = useRef(false);
  useEffect(() => {
    if (pending || left.current) return;
    left.current = true;
    navigate(afterSignInPath(), { replace: true });
  }, [pending, navigate]);
  useEffect(() => {
    if (error) pendingInvitation.clear();
  }, [error]);
  if (error) return <ErrorNotice error={error} />;
  return isLoading ? <Loading label="Signing in" /> : null;
}

/** Route `callback` with the mock API. */
export function MockCallbackPage() {
  const { navigate } = useRouter();
  useEffect(() => {
    navigate(afterSignInPath(), { replace: true });
  }, [navigate]);
  return <Loading label="Signing in" />;
}
