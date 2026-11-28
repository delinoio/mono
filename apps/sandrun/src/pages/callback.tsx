// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect, useRef, useState } from "react";
import { logtoSignInSessionState, SignInSessionState } from "../auth.tsx";
import { SignedOutTopBar } from "../components/shell.tsx";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { useI18n } from "../i18n.ts";
import { RouteId, routePath, useRouter } from "../router.tsx";

const UNREADABLE_SESSION = new Error(
  "The saved sign-in session is not readable",
);

/** Complete a pending redirect, show its error notice, or leave an idle callback. */
export function LogtoCallbackPage({ appId }: { appId: string }) {
  // Capture the state before the SDK can complete and remove the session.
  const [state] = useState(() =>
    logtoSignInSessionState(appId, window.location),
  );
  // The SDK checks the session before its callback error handler runs.
  if (state === SignInSessionState.Invalid) {
    return <CallbackError error={UNREADABLE_SESSION} />;
  }
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

/** A failed sign-in, with the language menu of the signed-out pages. */
function CallbackError({ error }: { error: unknown }) {
  return (
    <>
      <SignedOutTopBar />
      <ErrorNotice error={error} />
    </>
  );
}

function CompleteSignIn() {
  const { t } = useI18n();
  const { navigate } = useRouter();
  const { isLoading, error } = useHandleSignInCallback(() => {
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  });
  if (error) return <CallbackError error={error} />;
  return isLoading ? <Loading label={t("signIn.signingIn")} /> : null;
}

/** Route `callback` with the mock API. */
export function MockCallbackPage() {
  const { t } = useI18n();
  const { navigate } = useRouter();
  useEffect(() => {
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  }, [navigate]);
  return <Loading label={t("signIn.signingIn")} />;
}
