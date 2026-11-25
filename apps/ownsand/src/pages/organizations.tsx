// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Badge } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { type FormEvent, useState } from "react";
import { TopBar } from "../components/shell.tsx";
import {
  Empty,
  FormDialog,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import { labels } from "../format.ts";
import { secretFromInput, SECRET_PARAM } from "../invitation.ts";
import { refreshOrganizations, useOrganizations } from "../organizations.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

const organizationIdPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

/** Route `organizations`: the organization list, creation and invitations. */
export function OrganizationsPage() {
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const orgs = useOrganizations();
  const create = useMutation(OrganizationService.method.createOrganization);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [link, setLink] = useState("");
  const [linkError, setLinkError] = useState<string>();

  const join = (event: FormEvent) => {
    event.preventDefault();
    const secret = secretFromInput(link);
    if (secret === undefined) {
      setLinkError("Paste the whole invitation link.");
      return;
    }
    setLink("");
    navigate(
      `${routePath({ id: RouteId.Invitation })}#${SECRET_PARAM}=${encodeURIComponent(secret)}`,
    );
  };

  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <PageHeader
          title="Organizations"
          description="Choose an organization to manage its devices and sandboxes."
          action={
            <Button onClick={() => setCreating(true)}>
              Create organization
            </Button>
          }
        />
        <QueryBoundary query={orgs} loading="Loading organizations">
          {(organizations) =>
            organizations.length === 0 && !orgs.isIncomplete ? (
              <Empty title="You are not in an organization yet">
                Create one, or open an invitation link from an admin.
              </Empty>
            ) : (
              <>
                <ul className="list" aria-label="Your organizations">
                  {organizations.map((org) => (
                    <li key={org.organizationId} className="list-row">
                      <Link
                        className="list-link"
                        to={routePath({
                          id: RouteId.Sandboxes,
                          organizationId: org.organizationId,
                        })}
                      >
                        <span className="list-title">{org.name}</span>
                        <Badge>{labels.role[org.callerRole]}</Badge>
                      </Link>
                    </li>
                  ))}
                </ul>
                <MorePagesNotice noun="organizations" pages={orgs} />
              </>
            )
          }
        </QueryBoundary>
        <section className="section" aria-labelledby="join-title">
          <h2 id="join-title">Join with an invitation</h2>
          <form className="inline-form" onSubmit={join}>
            <TextField
              label="Invitation link"
              value={link}
              onValueChange={(value) => {
                setLink(value);
                setLinkError(undefined);
              }}
              error={linkError}
              autoComplete="off"
            />
            <Button type="submit" variant={ButtonVariant.Secondary}>
              Continue
            </Button>
          </form>
        </section>
      </main>
      <FormDialog
        open={creating}
        title="Create an organization"
        description="You become its owner. You can invite members after you create it."
        submitLabel="Create"
        onOpenChange={(open) => {
          setCreating(open);
          if (!open) setName("");
        }}
        onSubmit={async (opening) => {
          const res = await create.mutateAsync({ name: name.trim() });
          // The list and the switcher refresh in the background. The console
          // opens the new organization from the `CreateOrganization`
          // response instead of waiting for the list refetch.
          void refreshOrganizations(queryClient);
          // A request of a dismissed opening neither closes the form of a
          // later opening nor moves the user away from the page they went to.
          if (!opening.isCurrent()) return;
          const organizationId = res.organization?.organizationId;
          if (!organizationId || !organizationIdPattern.test(organizationId)) {
            throw new ConnectError(
              "The organization may have been created. Close this dialog and check the organization list before trying again.",
              Code.FailedPrecondition,
            );
          }
          setCreating(false);
          setName("");
          navigate(
            routePath({
              id: RouteId.Sandboxes,
              organizationId,
            }),
          );
        }}
      >
        <TextField
          label="Organization name"
          value={name}
          onValueChange={setName}
          required
          autoFocus
          maxLength={100}
        />
      </FormDialog>
    </>
  );
}
