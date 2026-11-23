// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect, useRef, useState } from "react";
import { Button, ButtonVariant } from "delino-ui/button";
import {
  LogtoSignInRedirect,
  readLogtoSignInRedirect,
  useAuth,
} from "../auth.tsx";
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
 * the SDK finished the sign-in) leaves the route for the same target. A stored
 * sign-in session that cannot be read shows a failure with a way to sign in
 * again and deletes the pending invitation token.
 */
export function LogtoCallbackPage({ appId }: { appId: string }) {
  // Decide once on mount, before the hook completes a redirect and the SDK
  // removes its stored sign-in session.
  const [redirect] = useState(() =>
    readLogtoSignInRedirect(appId, window.location),
  );
  // The SDK throws for an unreadable session before the hook can report it,
  // so the hook must not mount for it.
  return redirect === LogtoSignInRedirect.Unreadable ? (
    <UnreadableSignInSession />
  ) : (
    <CompleteSignIn pending={redirect === LogtoSignInRedirect.Pending} />
  );
}

/** The failure for a stored sign-in session that the SDK cannot read. */
function UnreadableSignInSession() {
  const auth = useAuth();
  // The failed return ends the redirect, so the pending invitation token has
  // no reason to stay.
  useEffect(() => {
    pendingInvitation.clear();
  }, []);
  return (
    <main className="content content-narrow">
      <div className="notice notice-error" role="alert">
        <p>The sign-in could not be completed. Sign in again.</p>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => void auth.signIn()}
        >
          Sign in with Delino
        </Button>
      </div>
    </main>
  );
}

function CompleteSignIn({ pending }: { pending: boolean }) {
  const { navigate } = useRouter();
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
