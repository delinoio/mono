// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useInfiniteQuery,
  useMutation,
} from "@connectrpc/connect-query";
import { OrganizationService } from "@delino/ownsand-sdk/gen/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { Card, CardActions, CardDescription, CardTitle } from "delino-ui/card";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "delino-ui/dialog";
import { TextField } from "delino-ui/text-field";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/errors.ts";
import { PAGE_SIZE, pageOptions } from "../api/paging.ts";
import {
  EmptyState,
  ErrorState,
  LoadMore,
  LoadingState,
  PageHeader,
  formText,
} from "../components/common.tsx";
import { INVITATION_PATH, parseInvitationInput } from "../invitation.ts";
import { fromProtoRole, roleLabel } from "../roles.ts";
import { Link, useRouter } from "../router.tsx";
import { RouteId, organizationPath } from "../routes.ts";

function CreateOrganizationDialog({
  open,
  onClose,
}: {
  readonly open: boolean;
  readonly onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const { navigate } = useRouter();
  const [error, setError] = useState<string | undefined>(undefined);
  const create = useMutation(OrganizationService.method.createOrganization);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = formText(event.currentTarget, "name");
    if (name === "") {
      setError("Enter a name.");
      return;
    }
    setError(undefined);
    try {
      const response = await create.mutateAsync({ name });
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: OrganizationService,
          cardinality: undefined,
        }),
      });
      onClose();
      const organizationId = response.organization?.organizationId;
      if (organizationId) {
        navigate(organizationPath(organizationId, RouteId.Sandboxes));
      }
    } catch (caught) {
      setError(errorMessage(caught));
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          setError(undefined);
          onClose();
        }
      }}
    >
      <DialogContent>
        <form onSubmit={(event) => void submit(event)} noValidate>
          <DialogTitle>Create an organization</DialogTitle>
          <DialogDescription>
            You become its owner. You can invite members after you create it.
          </DialogDescription>
          <TextField
            label="Organization name"
            name="name"
            autoFocus
            required
            maxLength={100}
            error={error}
          />
          <DialogActions>
            <DialogClose>Cancel</DialogClose>
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? "Creating…" : "Create"}
            </Button>
          </DialogActions>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function InvitationEntry({ focus }: { readonly focus: boolean }) {
  const { navigate } = useRouter();
  const [error, setError] = useState<string | undefined>(undefined);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const secret = parseInvitationInput(formText(form, "invitation"));
    if (secret === undefined) {
      setError("Paste the whole invitation link you received.");
      return;
    }
    setError(undefined);
    form.reset();
    // The acceptance route reads the fragment and removes it at once.
    navigate(
      `${INVITATION_PATH}#${new URLSearchParams({ secret }).toString()}`,
    );
  };
  return (
    <Card className="invitation-entry">
      <CardTitle>Join an organization</CardTitle>
      <CardDescription>
        Paste the invitation link that an organization admin shared with you.
      </CardDescription>
      <form
        onSubmit={(event) => void submit(event)}
        noValidate
        className="inline-form"
      >
        <TextField
          label="Invitation link"
          name="invitation"
          autoComplete="off"
          // The single input is the main form only when the user has no
          // organization yet.
          autoFocus={focus}
          error={error}
        />
        <CardActions>
          <Button type="submit" variant={ButtonVariant.Secondary}>
            Continue
          </Button>
        </CardActions>
      </form>
    </Card>
  );
}

export function OrganizationsPage() {
  const [creating, setCreating] = useState(false);
  const query = useInfiniteQuery(
    OrganizationService.method.listOrganizations,
    { pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const organizations =
    query.data?.pages.flatMap((page) => page.organizations) ?? [];

  return (
    <main className="page">
      <PageHeader
        title="Organizations"
        description="Choose an organization to manage its devices and sandboxes."
        actions={
          <Button onClick={() => setCreating(true)}>Create organization</Button>
        }
      />
      {query.isPending ? (
        <LoadingState />
      ) : query.isError ? (
        <ErrorState error={query.error} />
      ) : organizations.length === 0 ? (
        <EmptyState>
          You are not a member of any organization yet. Create one, or join one
          with an invitation link.
        </EmptyState>
      ) : (
        <ul className="card-list" aria-label="Your organizations">
          {organizations.map((organization) => (
            <li key={organization.organizationId}>
              <Link
                className="card-link"
                to={organizationPath(
                  organization.organizationId,
                  RouteId.Sandboxes,
                )}
              >
                <span className="card-link-title">{organization.name}</span>
                <span className="muted">
                  {roleLabel(fromProtoRole(organization.callerRole))}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
      <LoadMore
        hasMore={query.hasNextPage}
        loading={query.isFetchingNextPage}
        onLoadMore={() => void query.fetchNextPage()}
      />
      <InvitationEntry focus={query.isSuccess && organizations.length === 0} />
      <CreateOrganizationDialog
        open={creating}
        onClose={() => setCreating(false)}
      />
    </main>
  );
}
