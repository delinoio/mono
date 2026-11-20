// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useInfiniteQuery,
  useMutation,
} from "@connectrpc/connect-query";
import { ApiKeyState } from "@delino/ownsand-sdk/gen/common_pb";
import {
  ApiKeyService,
  type ApiKey,
} from "@delino/ownsand-sdk/gen/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
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
import { ConfirmDialog } from "../components/confirm-dialog.tsx";
import { SecretDialog } from "../components/secret-dialog.tsx";
import { formatEnum, formatTime } from "../format.ts";
import type { OrganizationPageProps } from "./organization-layout.tsx";

function CreateApiKeyDialog({
  open,
  onClose,
  onCreate,
}: {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onCreate: (name: string) => Promise<void>;
}) {
  const [error, setError] = useState<string | undefined>(undefined);
  const [pending, setPending] = useState(false);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = formText(event.currentTarget, "name");
    if (name === "") {
      setError("Enter a name.");
      return;
    }
    setError(undefined);
    setPending(true);
    try {
      await onCreate(name);
      onClose();
    } catch (caught) {
      setError(errorMessage(caught));
    } finally {
      setPending(false);
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
          <DialogTitle>Create an API key</DialogTitle>
          <DialogDescription>
            Programs such as CI jobs and AI agents use API keys to manage
            sandboxes in this organization.
          </DialogDescription>
          <TextField
            label="Key name"
            name="name"
            description="For example, the program or job that uses it."
            autoFocus
            required
            maxLength={100}
            error={error}
          />
          <DialogActions>
            <DialogClose>Cancel</DialogClose>
            <Button type="submit" disabled={pending}>
              {pending ? "Creating…" : "Create key"}
            </Button>
          </DialogActions>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function ApiKeysPage({ organizationId }: OrganizationPageProps) {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<ApiKey | undefined>(undefined);
  const [newSecret, setNewSecret] = useState<string | undefined>(undefined);
  const query = useInfiniteQuery(
    ApiKeyService.method.listApiKeys,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const create = useMutation(ApiKeyService.method.createApiKey);
  const revoke = useMutation(ApiKeyService.method.revokeApiKey);
  const invalidate = () =>
    queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({
        schema: ApiKeyService,
        cardinality: undefined,
      }),
    });
  const keys = query.data?.pages.flatMap((page) => page.apiKeys) ?? [];

  return (
    <main className="page">
      <PageHeader
        title="API keys"
        description="Keys for programs. A key can use devices and sandboxes of this organization, but cannot read its members."
        actions={
          <Button onClick={() => setCreating(true)}>Create API key</Button>
        }
      />
      {query.isPending ? (
        <LoadingState />
      ) : query.isError ? (
        <ErrorState error={query.error} />
      ) : keys.length === 0 ? (
        <EmptyState>No API keys yet.</EmptyState>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Starts with</th>
                <th scope="col">State</th>
                <th scope="col">Created</th>
                <th scope="col">
                  <span className="visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {keys.map((key) => (
                <tr key={key.apiKeyId}>
                  <td>{key.name}</td>
                  <td className="mono">{key.secretPrefix}…</td>
                  <td>{formatEnum(ApiKeyState, key.state)}</td>
                  <td>{formatTime(key.createTime)}</td>
                  <td className="row-actions">
                    {key.state === ApiKeyState.ACTIVE ? (
                      <Button
                        size={ButtonSize.Small}
                        variant={ButtonVariant.Danger}
                        aria-label={`Revoke ${key.name}`}
                        onClick={() => setRevoking(key)}
                      >
                        Revoke
                      </Button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <LoadMore
        hasMore={query.hasNextPage}
        loading={query.isFetchingNextPage}
        onLoadMore={() => void query.fetchNextPage()}
      />
      <CreateApiKeyDialog
        open={creating}
        onClose={() => setCreating(false)}
        onCreate={async (name) => {
          const response = await create.mutateAsync({ organizationId, name });
          await invalidate();
          setNewSecret(response.apiKeySecret);
        }}
      />
      <ConfirmDialog
        open={revoking !== undefined}
        title={`Revoke ${revoking?.name ?? "this API key"}?`}
        description="Programs that use this key stop working at once. This cannot be undone."
        confirmLabel="Revoke API key"
        onClose={() => setRevoking(undefined)}
        onConfirm={async () => {
          if (revoking === undefined) {
            return;
          }
          await revoke.mutateAsync({
            organizationId,
            apiKeyId: revoking.apiKeyId,
          });
          await invalidate();
        }}
      />
      <SecretDialog
        secret={newSecret}
        title="API key"
        description="Use this key in the Authorization header of your program."
        secretLabel="API key"
        onClose={() => setNewSecret(undefined)}
      />
    </main>
  );
}
