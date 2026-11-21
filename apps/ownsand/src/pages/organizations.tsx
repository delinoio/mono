// SPDX-License-Identifier: Apache-2.0
import { LIST_ORGANIZATIONS_REQUEST } from "../api.ts";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { Badge } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { OrganizationService } from "ownsand-sdk/gen/organization_pb";
import { type FormEvent, useState } from "react";
import { TopBar } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  FormDialog,
  Loading,
  PageHeader,
} from "../components/ui.tsx";
import { labels } from "../format.ts";
import { secretFromInput, SECRET_PARAM } from "../invitation.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/** Route `organizations`: the organization list, creation and invitations. */
export function OrganizationsPage() {
  const { navigate } = useRouter();
  const orgs = useQuery(
    OrganizationService.method.listOrganizations,
    LIST_ORGANIZATIONS_REQUEST,
  );
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
        {orgs.isPending ? (
          <Loading label="Loading organizations" />
        ) : orgs.error ? (
          <ErrorNotice error={orgs.error} onRetry={() => void orgs.refetch()} />
        ) : orgs.data.organizations.length === 0 ? (
          <Empty title="You are not in an organization yet">
            Create one, or open an invitation link from an admin.
          </Empty>
        ) : (
          <ul className="list" aria-label="Your organizations">
            {orgs.data.organizations.map((org) => (
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
        )}
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
        onSubmit={async () => {
          const res = await create.mutateAsync({ name: name.trim() });
          await orgs.refetch();
          setCreating(false);
          setName("");
          navigate(
            routePath({
              id: RouteId.Sandboxes,
              organizationId: res.organization!.organizationId,
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
