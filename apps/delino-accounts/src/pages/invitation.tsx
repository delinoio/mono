// SPDX-License-Identifier: Apache-2.0
import { createConnectQueryKey, useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { useEffect, useRef, useState } from "react";
import { useAuth } from "../auth.tsx";
import { Page } from "../components/shell.tsx";
import {
  ErrorNotice,
  PageHeader,
  SignInFailureNotice,
} from "../components/ui.tsx";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { useI18n } from "../i18n.ts";
import {
  openInvitationFromAddress,
  pendingInvitation,
  tokenFromFragment,
} from "../invitation.ts";
import { Link, parseRoute, RouteId, routePath, useRouter } from "../router.tsx";

/**
 * Route `billing-invitation`: accepts a billing account invitation as the
 * signed-in user. The token comes from the URL fragment, which the page
 * removes at once, or from `sessionStorage` after the sign-in redirect.
 */
export function InvitationPage({ configured }: { configured: boolean }) {
  const auth = useAuth();
  const { navigate } = useRouter();
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const [token, setToken] = useState<string | undefined>(() => {
    const fromAddress = openInvitationFromAddress(
      window.location,
      window.history,
    );
    return fromAddress ?? pendingInvitation.read();
  });
  const [needsRecovery, setNeedsRecovery] = useState(false);
  const accept = useMutation(BillingAccountService.method.acceptInvitation);
  const resetAccept = accept.reset;
  const signingIn = useRef(false);
  // Identifies the invitation attempt that the page shows. A new link or the
  // unmount of the page replaces it, so the late success of an earlier
  // request does not navigate away from what the user sees.
  const currentAttempt = useRef(0);
  // Keys the accept button by invitation attempt. The button keeps its
  // pending state until the click promise of the request that it sent
  // settles, so a replacement link remounts it. Without that, the button of
  // the new link would stay pending behind the request of the earlier link.
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    return () => {
      currentAttempt.current += 1;
    };
  }, []);

  useEffect(() => {
    if (auth.signInFailed) {
      signingIn.current = false;
    }
  }, [auth.signInFailed]);

  useEffect(() => {
    // A new link opened while the page is shown.
    const onHash = () => {
      const next = openInvitationFromAddress(window.location, window.history);
      if (next !== undefined) {
        if (signingIn.current) {
          pendingInvitation.save(next);
        }
        currentAttempt.current += 1;
        resetAccept();
        setAttempt((n) => n + 1);
        setToken(next);
        setNeedsRecovery(false);
      }
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, [resetAccept]);

  if (token === undefined) {
    return (
      <Page narrow>
        <PageHeader
          title={t("invitation.missing.title")}
          description={t("invitation.missing.description")}
        />
        <Link to={routePath({ id: RouteId.Billing })}>
          {t("common.goToBillingAccounts")}
        </Link>
      </Page>
    );
  }

  if (needsRecovery) {
    return (
      <Page narrow>
        <PageHeader title={t("invitation.join.title")} />
        <div className="notice">
          <p role="alert">{t("invitation.mayHaveJoined")}</p>
          <Link to={routePath({ id: RouteId.Billing })}>
            {t("common.goToBillingAccounts")}
          </Link>
        </div>
      </Page>
    );
  }

  function signIn(invitationToken: string) {
    // The fragment does not survive the redirect. Keep the handoff active
    // until the page leaves or sign-in reports a failure.
    signingIn.current = true;
    pendingInvitation.save(invitationToken);
    return auth.signIn();
  }

  if (!auth.isAuthenticated) {
    return (
      <Page narrow>
        <PageHeader
          title={t("invitation.signIn.title")}
          description={t("invitation.signIn.description")}
        />
        {configured ? (
          <>
            <Button onClick={() => signIn(token)}>
              {t("invitation.signIn.action")}
            </Button>
            <SignInFailureNotice />
          </>
        ) : (
          <p role="alert">{t("auth.notConfigured")}</p>
        )}
      </Page>
    );
  }

  return (
    <Page narrow>
      <PageHeader
        title={t("invitation.join.title")}
        description={t("invitation.join.description")}
      />
      {accept.error ? (
        <ErrorNotice error={accept.error} onSignIn={() => signIn(token)} />
      ) : null}
      <div className="actions">
        <Button
          key={attempt}
          onClick={async () => {
            const clicked = currentAttempt.current;
            const response = await accept
              .mutateAsync({ invitationToken: token })
              .catch(() => undefined);
            if (response !== undefined) {
              // A successful request changes membership even after the user leaves.
              void queryClient.invalidateQueries({
                queryKey: createConnectQueryKey({
                  schema: BillingAccountService.method.listBillingAccounts,
                  cardinality: undefined,
                }),
              });
            }

            // The address can change before React unmounts or hashchange runs.
            // Leave a replacement fragment and its token for the listener.
            const { pathname, hash } = window.location;
            const addressToken = tokenFromFragment(hash);
            if (
              clicked !== currentAttempt.current ||
              parseRoute(pathname).id !== RouteId.BillingInvitation ||
              (addressToken !== undefined && addressToken !== token)
            ) {
              return;
            }
            pendingInvitation.clear();
            if (response === undefined) {
              // The error notice offers recovery. Keep the in-memory token for retry.
              return;
            }
            const billingAccountId =
              response.billingAccount?.billingAccountId.trim();
            if (!billingAccountId) {
              setNeedsRecovery(true);
              return;
            }
            navigate(
              routePath({ id: RouteId.BillingAccount, billingAccountId }),
              {
                replace: true,
              },
            );
          }}
        >
          {t("invitation.accept")}
        </Button>
        <Link to={routePath({ id: RouteId.Billing })}>
          {t("invitation.notNow")}
        </Link>
      </div>
    </Page>
  );
}
