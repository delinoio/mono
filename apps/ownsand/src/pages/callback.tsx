// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect, useRef, useState } from "react";
import { isLogtoSignInPending } from "../auth.tsx";
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
 * Route `callback` with Logto: completes the sign-in redirect. A visit with
 * no sign-in redirect in progress (a typed address, a bookmark or a reload
 * after the SDK finished the sign-in) leaves the route for the same target.
 * The error of a failed callback drops the pending invitation secret.
 */
export function LogtoCallbackPage({ appId }: { appId: string }) {
  const { navigate } = useRouter();
  // Decide once on mount, before the hook completes a redirect and the SDK
  // removes its stored sign-in session.
  const [pending] = useState(() =>
    isLogtoSignInPending(appId, window.location),
  );
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
