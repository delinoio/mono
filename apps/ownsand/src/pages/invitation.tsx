// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { Button } from "delino-ui/button";
import { MemberService } from "ownsand-sdk/gen/member_pb";
import { useEffect, useRef, useState } from "react";
import { useAuth } from "../auth.tsx";
import { ErrorNotice, PageHeader } from "../components/ui.tsx";
import { pendingInvitation, takeSecretFromAddress } from "../invitation.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/**
 * Route `invitation`: accepts an invitation secret as the signed-in user.
 * The secret comes from the URL fragment, which the page removes at once,
 * or from `sessionStorage` after the sign-in redirect.
 */
export function InvitationPage() {
  const auth = useAuth();
  const { navigate } = useRouter();
  const [secret, setSecret] = useState<string | undefined>(() => {
    const fromAddress = takeSecretFromAddress(window.location, window.history);
    return fromAddress ?? pendingInvitation.read();
  });
  const accept = useMutation(MemberService.method.acceptInvitation);
  const resetAccept = accept.reset;
  // Identifies the invitation attempt that the page currently shows. A new
  // link or the unmount of the page replaces it, so that the late success of
  // an earlier accept request does not navigate away from what the user sees.
  const currentAttempt = useRef(0);

  useEffect(() => {
    return () => {
      currentAttempt.current += 1;
    };
  }, []);

  useEffect(() => {
    // A new link opened while the page is shown. The error of the previous
    // link does not apply to it.
    const onHash = () => {
      const next = takeSecretFromAddress(window.location, window.history);
      if (next !== undefined) {
        currentAttempt.current += 1;
        resetAccept();
        setSecret(next);
      }
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, [resetAccept]);

  if (secret === undefined) {
    return (
      <main className="content content-narrow">
        <PageHeader
          title="Invitation"
          description="This page needs an invitation link. Ask an admin of the organization for a new one."
        />
        <Link to="/">Go to your organizations</Link>
      </main>
    );
  }

  if (!auth.isAuthenticated) {
    return (
      <main className="content content-narrow">
        <PageHeader
          title="You are invited to an OwnSand organization"
          description="Sign in to accept the invitation."
        />
        <Button
          onClick={async () => {
            // The fragment does not survive the sign-in redirect, so the
            // secret waits in sessionStorage until AcceptInvitation returns.
            pendingInvitation.save(secret);
            await auth.signIn();
          }}
        >
          Sign in to accept
        </Button>
      </main>
    );
  }

  return (
    <main className="content content-narrow">
      <PageHeader
        title="Join the organization"
        description="You join as the signed-in user. The invitation works only once."
      />
      {accept.error ? <ErrorNotice error={accept.error} /> : null}
      <div className="actions">
        <Button
          onClick={async () => {
            const attempt = currentAttempt.current;
            try {
              const res = await accept.mutateAsync({
                invitationSecret: secret,
              });
              // The server request is not aborted. Only a result that still
              // belongs to the shown invitation may change the route.
              if (attempt !== currentAttempt.current) {
                return;
              }
              navigate(
                routePath({
                  id: RouteId.Sandboxes,
                  organizationId: res.organization!.organizationId,
                }),
                { replace: true },
              );
            } catch {
              // accept.error shows the failure above. The in-memory secret
              // stays so that the user can accept again: the server leaves
              // the invitation unused when AcceptInvitation fails. Only the
              // stored copy is deleted (below).
            } finally {
              pendingInvitation.clear();
            }
          }}
        >
          Accept invitation
        </Button>
        <Link to="/">Not now</Link>
      </div>
    </main>
  );
}
