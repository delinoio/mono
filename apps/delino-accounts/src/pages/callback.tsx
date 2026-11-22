// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect, useRef, useState } from "react";
import { isLogtoSignInPending } from "../auth.tsx";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { pendingInvitation } from "../invitation.ts";
import { RouteId, routePath, useRouter } from "../router.tsx";

/** Where to go after sign-in: back to a pending invitation, or billing. */
export function afterSignInPath(): string {
  return pendingInvitation.read() !== undefined
    ? routePath({ id: RouteId.BillingInvitation })
    : routePath({ id: RouteId.Billing });
}

/**
 * Route `callback`: completes the Logto sign-in redirect. A visit with no
 * sign-in redirect in progress (a typed address, a bookmark or a reload after
 * the SDK finished the sign-in) leaves the route for the same target.
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
    if (pending || left.current) {
      return;
    }
    left.current = true;
    navigate(afterSignInPath(), { replace: true });
  }, [pending, navigate]);
  // A failed return ends the redirect, so the pending invitation token has no
  // reason to stay. A successful return keeps it until the callback navigates.
  useEffect(() => {
    if (error) {
      pendingInvitation.clear();
    }
  }, [error]);
  if (error) {
    return (
      <main className="content content-narrow">
        <ErrorNotice error={error} />
      </main>
    );
  }
  return isLoading ? <Loading label="Signing in" /> : null;
}
