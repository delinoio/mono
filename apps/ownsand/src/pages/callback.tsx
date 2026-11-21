// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect } from "react";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { pendingInvitation } from "../invitation.ts";
import { RouteId, routePath, useRouter } from "../router.tsx";

/** Where to go after sign-in: back to a pending invitation, or home. */
export function afterSignInPath(): string {
  return pendingInvitation.read() !== undefined
    ? routePath({ id: RouteId.Invitation })
    : routePath({ id: RouteId.Organizations });
}

/** Route `callback` with Logto: completes the sign-in redirect. */
export function LogtoCallbackPage() {
  const { navigate } = useRouter();
  const { isLoading, error } = useHandleSignInCallback(() => {
    navigate(afterSignInPath(), { replace: true });
  });
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
