// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect, useRef, useState } from "react";
import { logtoSignInSessionState, SignInSessionState } from "../auth.tsx";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { RouteId, routePath, useRouter } from "../router.tsx";

/** Route `callback` with Logto: completes a pending sign-in redirect or returns home. */
export function LogtoCallbackPage({ appId }: { appId: string }) {
  // Capture the state before the SDK can complete and remove the session.
  const [state] = useState(() =>
    logtoSignInSessionState(appId, window.location),
  );
  if (state === SignInSessionState.None) return <LeaveCallback />;
  return <CompleteSignIn />;
}

function LeaveCallback() {
  const { navigate } = useRouter();
  const left = useRef(false);
  useEffect(() => {
    // StrictMode reruns the effect and preserves the ref.
    if (left.current) return;
    left.current = true;
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  }, [navigate]);
  return null;
}

function CompleteSignIn() {
  const { navigate } = useRouter();
  const { isLoading, error } = useHandleSignInCallback(() => {
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  });
  if (error) return <ErrorNotice error={error} />;
  return isLoading ? <Loading label="Signing in" /> : null;
}

/** Route `callback` with the mock API. */
export function MockCallbackPage() {
  const { navigate } = useRouter();
  useEffect(() => {
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  }, [navigate]);
  return <Loading label="Signing in" />;
}
