// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { PasswordInput } from "delino-ui/password-input";
import { Button } from "delino-ui/button";
import { MemberService } from "@delino/tide-protos/v1/member_pb";
import { OrganizationService } from "@delino/tide-protos/v1/organization_pb";
import { type FormEvent, useEffect, useState } from "react";
import { invalidateServices } from "../api.ts";
import { TopBar } from "../components/shell.tsx";
import { ErrorNotice, PageHeader } from "../components/ui.tsx";
import { secretFromInput, takeSecretFromAddress } from "../invitation.ts";
import { orgPath, RouteId, useRouter } from "../router.tsx";

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
  const accept = useMutation(MemberService.method.acceptInvitation);
  useEffect(() => {
    takeSecretFromAddress(window.location, window.history);
  }, []);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const secret = secretFromInput(value);
    if (secret === undefined) {
      setInputError("Paste the invitation link or the secret.");
      return;
    }
    try {
      const res = await accept.mutateAsync({ invitationSecret: secret });
      setValue("");
      await invalidateServices(queryClient, OrganizationService);
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
