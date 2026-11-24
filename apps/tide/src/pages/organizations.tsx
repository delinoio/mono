// SPDX-License-Identifier: Apache-2.0
import { LIST_ORGANIZATIONS_REQUEST } from "../api.ts";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { Badge } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { OrganizationService } from "@delino/tide-protos/v1/organization_pb";
import { type FormEvent, useState } from "react";
import { TopBar } from "../components/shell.tsx";
import {
  Empty,
  FormDialog,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import { labels } from "../format.ts";
import { secretFromInput, SECRET_PARAM } from "../invitation.ts";
import { Link, orgPath, RouteId, routePath, useRouter } from "../router.tsx";

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
      setLinkError("Paste the whole invitation link or the secret.");
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
          description="Choose an organization to see its queue, runners and devices."
          action={
            <Button onClick={() => setCreating(true)}>
              Create organization
            </Button>
          }
        />
        <QueryBoundary query={orgs} loading="Loading organizations">
          {(data) =>
            data.organizations.length === 0 ? (
              <Empty title="You are not in an organization yet">
                Create one, or open an invitation link from an admin. Each tide
                organization links to one OwnSand organization.
              </Empty>
            ) : (
              <ul className="list" aria-label="Your organizations">
                {data.organizations.map((org) => (
                  <li key={org.organizationId} className="list-row">
                    <Link
                      className="list-link"
                      to={orgPath(RouteId.Overview, org.organizationId)}
                    >
                      <span className="list-title">{org.name}</span>
                      <Badge>{labels.role[org.callerRole]}</Badge>
                    </Link>
                  </li>
                ))}
              </ul>
            )
          }
        </QueryBoundary>
        <section className="section" aria-labelledby="join-title">
          <h2 id="join-title">Join with an invitation</h2>
          <form className="inline-form" onSubmit={join}>
            <TextField
              label="Invitation link or secret"
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
        description="You become its owner. Next, link it to an OwnSand organization and invite members."
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
          navigate(orgPath(RouteId.Overview, res.organization!.organizationId));
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
