// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { PasswordInput } from "delino-ui/password-input";
import { Button } from "delino-ui/button";
import { MemberService } from "@delino/tide-protos/v1/member_pb";
import { OrganizationService } from "@delino/tide-protos/v1/organization_pb";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { TopBar } from "../components/shell.tsx";
import { ErrorNotice, PageHeader } from "../components/ui.tsx";
import { secretFromInput, takeSecretFromAddress } from "../invitation.ts";
import { orgPath, parseRoute, RouteId, useRouter } from "../router.tsx";

/**
 * Route `invitation`: accept an invitation secret as the signed-in user. The
 * secret comes from the link fragment, which the page removes from the
 * address bar at once, or from the input. It stays in component state only.
 */
export function InvitationPage() {
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const [value, setValue] = useState(
    () => secretFromInput(window.location.hash) ?? "",
  );
  const [inputError, setInputError] = useState<string>();
  const currentAttempt = useRef(0);
  const accept = useMutation(MemberService.method.acceptInvitation, {
    // Joining changes the organization list even if the user has moved on.
    onSuccess: () => invalidateServices(queryClient, OrganizationService),
  });
  useEffect(() => {
    takeSecretFromAddress(window.location, window.history);
    return () => {
      currentAttempt.current += 1;
    };
  }, []);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const secret = secretFromInput(value);
    if (secret === undefined) {
      setInputError("Paste the invitation link or the secret.");
      return;
    }
    const attempt = ++currentAttempt.current;
    try {
      const res = await accept.mutateAsync({ invitationSecret: secret });
      // The browser can change routes before React unmounts this page.
      if (
        attempt !== currentAttempt.current ||
        parseRoute(window.location.pathname).id !== RouteId.Invitation
      ) {
        return;
      }
      setValue("");
      navigate(orgPath(RouteId.Overview, res.organization!.organizationId), {
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
          title="Accept an invitation"
          description="Join a tide organization with the one-time invitation that an admin sent you. An invitation works for 7 days."
        />
        <form className="form" onSubmit={submit} noValidate>
          <PasswordInput
            label="Invitation link or secret"
            toggleLabel="Show invitation"
            value={value}
            onValueChange={(v: string) => {
              currentAttempt.current += 1;
              setValue(v);
              setInputError(undefined);
            }}
            error={inputError}
            autoFocus
            autoComplete="off"
          />
          {accept.error ? <ErrorNotice error={accept.error} /> : null}
          <div className="actions">
            <Button type="submit" disabled={accept.isPending}>
              Join organization
            </Button>
          </div>
        </form>
      </main>
    </>
  );
}
