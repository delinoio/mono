// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { Button } from "delino-ui/button";
import { MemberService } from "ownsand-sdk/gen/member_pb";
import { useEffect, useState } from "react";
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

  useEffect(() => {
    // A new link opened while the page is shown.
    const onHash = () => {
      const next = takeSecretFromAddress(window.location, window.history);
      if (next !== undefined) setSecret(next);
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

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
            try {
              const res = await accept.mutateAsync({
                invitationSecret: secret,
              });
              navigate(
                routePath({
                  id: RouteId.Sandboxes,
                  organizationId: res.organization!.organizationId,
                }),
                { replace: true },
              );
            } catch {
              setSecret(undefined);
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
