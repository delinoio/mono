// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { Button } from "delino-ui/button";
import { useEffect, useRef, useState } from "react";
import { useAuth } from "../auth.tsx";
import { Page } from "../components/shell.tsx";
import { ErrorNotice, PageHeader } from "../components/ui.tsx";
import { BillingAccountService } from "../gen/delino/accounts/v1/billing_account_pb.ts";
import { pendingInvitation, takeTokenFromAddress } from "../invitation.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/**
 * Route `billing-invitation`: accepts a billing account invitation as the
 * signed-in user. The token comes from the URL fragment, which the page
 * removes at once, or from `sessionStorage` after the sign-in redirect.
 */
export function InvitationPage({ configured }: { configured: boolean }) {
  const auth = useAuth();
  const { navigate } = useRouter();
  const [token, setToken] = useState<string | undefined>(() => {
    const fromAddress = takeTokenFromAddress(window.location, window.history);
    return fromAddress ?? pendingInvitation.read();
  });
  const accept = useMutation(BillingAccountService.method.acceptInvitation);
  const resetAccept = accept.reset;
  // Identifies the invitation attempt that the page shows. A new link or the
  // unmount of the page replaces it, so the late success of an earlier
  // request does not navigate away from what the user sees.
  const currentAttempt = useRef(0);

  useEffect(() => {
    return () => {
      currentAttempt.current += 1;
    };
  }, []);

  useEffect(() => {
    // A new link opened while the page is shown.
    const onHash = () => {
      const next = takeTokenFromAddress(window.location, window.history);
      if (next !== undefined) {
        currentAttempt.current += 1;
        resetAccept();
        setToken(next);
      }
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, [resetAccept]);

  if (token === undefined) {
    return (
      <Page narrow>
        <PageHeader
          title="Invitation"
          description="This page needs the link from an invitation email. Ask an owner or admin of the billing account to invite you again."
        />
        <Link to={routePath({ id: RouteId.Billing })}>
          Go to your billing accounts
        </Link>
      </Page>
    );
  }

  if (!auth.isAuthenticated) {
    return (
      <Page narrow>
        <PageHeader
          title="You are invited to a billing account"
          description="Sign in with the email address that received the invitation to accept it."
        />
        {configured ? (
          <Button
            onClick={async () => {
              // The fragment does not survive the sign-in redirect, so the
              // token waits in sessionStorage until AcceptInvitation returns.
              pendingInvitation.save(token);
              await auth.signIn();
            }}
          >
            Sign in to accept
          </Button>
        ) : (
          <p role="alert">Sign-in is not configured for this build.</p>
        )}
      </Page>
    );
  }

  return (
    <Page narrow>
      <PageHeader
        title="Join the billing account"
        description="You join as the signed-in user. The invitation works only once and only for the invited email address."
      />
      {accept.error ? <ErrorNotice error={accept.error} /> : null}
      <div className="actions">
        <Button
          onClick={async () => {
            const attempt = currentAttempt.current;
            try {
              const res = await accept.mutateAsync({
                invitationToken: token,
              });
              if (attempt !== currentAttempt.current) {
                return;
              }
              navigate(
                routePath({
                  id: RouteId.BillingAccount,
                  billingAccountId: res.billingAccount!.billingAccountId,
                }),
                { replace: true },
              );
            } catch {
              // accept.error shows the failure above. The in-memory token
              // stays so that the user can try again after, for example,
              // signing in with the invited address.
            } finally {
              pendingInvitation.clear();
            }
          }}
        >
          Accept invitation
        </Button>
        <Link to={routePath({ id: RouteId.Billing })}>Not now</Link>
      </div>
    </Page>
  );
}
