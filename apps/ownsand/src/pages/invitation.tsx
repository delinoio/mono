// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { useEffect, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { SIGN_IN_NOT_CONFIGURED, useAuth } from "../auth.tsx";
import {
  ErrorNotice,
  PageHeader,
  SignInFailureNotice,
} from "../components/ui.tsx";
import {
  openInvitationFromAddress,
  pendingInvitation,
  takeSecretFromAddress,
} from "../invitation.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/**
 * Route `invitation`: accepts an invitation secret as the signed-in user.
 * The secret comes from the URL fragment, which the page removes at once,
 * or from `sessionStorage` after the sign-in redirect. A build without a
 * sign-in configuration cannot sign the user in, so the page shows the
 * not-configured notice and never stores the secret.
 */
export function InvitationPage({ configured }: { configured: boolean }) {
  const auth = useAuth();
  const { navigate } = useRouter();
  const [invitation, setInvitation] = useState(() => ({
    secret:
      openInvitationFromAddress(window.location, window.history) ??
      pendingInvitation.read(),
    generation: 0,
  }));
  const { secret, generation } = invitation;
  const currentInvitation = useRef(invitation);
  const queryClient = useQueryClient();
  const accept = useMutation(MemberService.method.acceptInvitation, {
    // Joining changes the caller's organizations and their members, so the
    // cached lists must not outlive the 10 second stale time: the switcher of
    // the new organization has to list it. This runs for every success,
    // including the late success of an earlier attempt that no longer
    // navigates. The promise is not returned: TanStack Query would wait for it
    // before `mutateAsync` resolves, so the navigation would wait for the
    // refetch of every query that is active at that moment.
    onSuccess: () => {
      void invalidateServices(queryClient, MemberService, OrganizationService);
    },
  });
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
    const onHash = () => {
      const secret = takeSecretFromAddress(window.location, window.history);
      if (secret === undefined || secret === currentInvitation.current.secret) {
        return;
      }
      // A different link gets its own button and mutation state. The
      // previous button may still be waiting for its request to settle.
      currentAttempt.current += 1;
      const next = { secret, generation: currentAttempt.current };
      currentInvitation.current = next;
      pendingInvitation.clear();
      resetAccept();
      setInvitation(next);
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

  if (!auth.isAuthenticated && !configured) {
    // The secret stays in memory only: there is no sign-in redirect that it
    // would have to survive. The fragment is already gone from the address.
    return (
      <main className="content content-narrow">
        <PageHeader title="You are invited to an OwnSand organization" />
        <p role="alert">{SIGN_IN_NOT_CONFIGURED}</p>
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
        <SignInFailureNotice />
      </main>
    );
  }

  return (
    <main className="content content-narrow">
      <PageHeader
        title="Join the organization"
        description="You join as the signed-in user. The invitation works only once."
      />
      {accept.error ? (
        <ErrorNotice
          error={accept.error}
          onSignIn={async () => {
            // The accept handler cleared the stored copy after the failure.
            // Store the secret again so that it survives the sign-in redirect.
            pendingInvitation.save(secret);
            await auth.signIn();
          }}
        />
      ) : null}
      <div className="actions">
        <Button
          key={generation}
          onClick={async () => {
            if (invitation !== currentInvitation.current) {
              return;
            }
            const attempt = currentAttempt.current;
            // Whether the attempt still belongs to the shown invitation,
            // read the moment its request settles: the navigation below
            // unmounts the page and so ends the attempt, and a current
            // success must still delete its stored copy.
            let current = false;
            try {
              let res;
              try {
                res = await accept.mutateAsync({ invitationSecret: secret });
              } finally {
                current = attempt === currentAttempt.current;
              }
              // The server request is not aborted. Only a result that still
              // belongs to the shown invitation may change the route.
              if (!current) {
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
              // The stored secret may belong to a newer link whose sign-in
              // redirect is pending. A settled request of an earlier link
              // must not delete it.
              if (current) {
                pendingInvitation.clear();
              }
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
