// SPDX-License-Identifier: Apache-2.0
import { createConnectQueryKey, useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { PasswordInput } from "delino-ui/password-input";
import { Button } from "delino-ui/button";
import { MemberService } from "@delino/sandrun-protos/v1/member_pb";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { TopBar } from "../components/shell.tsx";
import { ErrorNotice, PageHeader } from "../components/ui.tsx";
import {
  secretFromFragment,
  secretFromInput,
  takeSecretFromAddress,
} from "../invitation.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { refreshOrganizations } from "../organizations.ts";
import { Link, orgPath, parseRoute, RouteId, useRouter } from "../router.tsx";

/**
 * Accepts an invitation as the signed-in user. The form starts with the
 * secret captured on entry and can receive a replacement link or typed input.
 */
export function InvitationPage({
  initialSecret,
}: {
  initialSecret: string | undefined;
}) {
  const { t } = useI18n();
  const { isCurrentVisit, navigate } = useRouter();
  const queryClient = useQueryClient();
  const [value, setValue] = useState(initialSecret ?? "");
  const [inputError, setInputError] = useState<MessageKey>();
  const [showRecovery, setShowRecovery] = useState(false);
  const currentAttempt = useRef(0);
  const accept = useMutation(MemberService.method.acceptInvitation, {
    // Joining changes the organization list even if the user has moved on.
    onSuccess: async () => {
      await Promise.all([
        refreshOrganizations(queryClient),
        queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({
            schema: OrganizationService.method.getOrganization,
            cardinality: undefined,
          }),
        }),
      ]);
    },
  });
  const resetAccept = accept.reset;
  useEffect(() => {
    const consumeInvitation = () => {
      if (parseRoute(window.location.pathname).id !== RouteId.Invitation) {
        return;
      }
      const secret = takeSecretFromAddress(window.location, window.history);
      if (secret === undefined) {
        return;
      }
      currentAttempt.current += 1;
      setValue(secret);
      setInputError(undefined);
      setShowRecovery(false);
      resetAccept();
    };
    window.addEventListener("hashchange", consumeInvitation);
    consumeInvitation();
    return () => {
      window.removeEventListener("hashchange", consumeInvitation);
      currentAttempt.current += 1;
    };
  }, [resetAccept]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const secret = secretFromInput(value);
    if (secret === undefined) {
      setInputError("invitation.inputRequired");
      return;
    }
    const attempt = ++currentAttempt.current;
    try {
      const res = await accept.mutateAsync({ invitationSecret: secret });
      // The address can change before React unmounts or hashchange runs.
      if (
        attempt !== currentAttempt.current ||
        !isCurrentVisit() ||
        secretFromFragment(window.location.hash) !== undefined
      ) {
        return;
      }
      setValue("");
      const organizationId = res.organization?.organizationId;
      if (!organizationId?.trim()) {
        setShowRecovery(true);
        return;
      }
      navigate(orgPath(RouteId.Overview, organizationId), {
        replace: true,
      });
    } catch {
      // The notice below shows the error.
    }
  };

  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <PageHeader
          title={t("invitation.title")}
          description={showRecovery ? undefined : t("invitation.description")}
        />
        {showRecovery ? (
          <div className="notice">
            <p role="alert">{t("invitation.mayHaveJoined")}</p>
            <Link to="/">{t("common.goToOrganizations")}</Link>
          </div>
        ) : (
          <form className="form" onSubmit={submit} noValidate>
            <PasswordInput
              label={t("invitation.inputLabel")}
              toggleLabel={t("invitation.showInput")}
              value={value}
              onValueChange={(v: string) => {
                currentAttempt.current += 1;
                setValue(v);
                setInputError(undefined);
                resetAccept();
              }}
              error={inputError && t(inputError)}
              autoFocus
              autoComplete="off"
            />
            {accept.error ? <ErrorNotice error={accept.error} /> : null}
            <div className="actions">
              <Button type="submit" disabled={accept.isPending}>
                {t("invitation.join")}
              </Button>
            </div>
          </form>
        )}
      </main>
    </>
  );
}
