// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect } from "react";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { pendingInvitation } from "../invitation.ts";
import { RouteId, routePath, useRouter } from "../router.tsx";

/** Where to go after sign-in: back to a pending invitation, or billing. */
export function afterSignInPath(): string {
  return pendingInvitation.read() !== undefined
    ? routePath({ id: RouteId.BillingInvitation })
    : routePath({ id: RouteId.Billing });
}

/** Route `callback`: completes the Logto sign-in redirect. */
export function LogtoCallbackPage() {
  const { navigate } = useRouter();
  const { isLoading, error } = useHandleSignInCallback(() => {
    navigate(afterSignInPath(), { replace: true });
  });
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
