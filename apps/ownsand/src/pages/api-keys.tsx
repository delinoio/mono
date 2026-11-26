// SPDX-License-Identifier: Apache-2.0
import {
  createInfiniteQueryOptions,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { ApiKeyState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type ApiKey,
  ApiKeyService,
} from "@delino/ownsand-protos/v1/api_key_pb";
import { useMemo, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  FormDialog,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  isTerminalError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import {
  FOLLOW_ALL_PAGES_OPTIONS,
  boundaryOf,
  cancelUncachedList,
  uniqueRows,
  useFollowAllPages,
} from "../pagination.ts";

/** Route `api-keys`: create, list and revoke organization API keys. */
export function ApiKeysPage() {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const transport = useTransport();
  const refresh = () => invalidateServices(queryClient, ApiKeyService);
  // The control plane lists keys in ascending ID order, so a new key is on the
  // last page. Read every page, or it could not be revoked from here.
  const listOptions = (listOrganizationId: string) =>
    createInfiniteQueryOptions(
      ApiKeyService.method.listApiKeys,
      { organizationId: listOrganizationId, pageSize: 200, pageToken: "" },
      {
        transport,
        pageParamKey: FOLLOW_ALL_PAGES_OPTIONS.pageParamKey,
        getNextPageParam: (last) => last.nextPageToken || undefined,
      },
    );
  const list = useInfiniteQuery({
    ...listOptions(organizationId),
    ...FOLLOW_ALL_PAGES_OPTIONS,
  });
  const pages = useFollowAllPages(list);
  const apiKeys = useMemo(
    () =>
      uniqueRows(
        list.data?.pages.map((page) => page.apiKeys) ?? [],
        (k) => k.apiKeyId,
      ),
    [list.data],
  );
  const create = useMutation(ApiKeyService.method.createApiKey, {
    onSuccess: (_, { organizationId: submittedOrganizationId }) => {
      if (submittedOrganizationId === undefined) {
        void refresh();
        return;
      }
      void cancelUncachedList(
        queryClient,
        listOptions(submittedOrganizationId).queryKey,
      ).then(refresh);
    },
  });
  const revoke = useMutation(ApiKeyService.method.revokeApiKey, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [nameError, setNameError] = useState<string>();
  const [secret, setSecret] = useState<string>();
  const [revoking, setRevoking] = useState<ApiKey>();

  if (isTerminalError(pages.error) && revoking !== undefined) {
    setRevoking(undefined);
  }

  return (
    <>
      <PageHeader
        title="API keys"
        description="Programs such as CI jobs and AI agents use API keys to manage sandboxes. A key cannot read members."
        action={
          <Button onClick={() => setCreating(true)}>Create API key</Button>
        }
      />
      <QueryBoundary query={boundaryOf(list, pages)} loading="Loading API keys">
        {() =>
          apiKeys.length === 0 && !pages.isIncomplete ? (
            <Empty title="No API keys" />
          ) : (
            <>
              {apiKeys.length > 0 ? (
                <ul className="list" aria-label="API keys">
                  {apiKeys.map((k) => (
                    <li key={k.apiKeyId} className="list-row">
                      <div className="list-main">
                        <span className="list-title">
                          {k.name} · {labels.apiKeyState[k.state]}
                        </span>
                        <span className="muted">
                          <code>{k.keyPrefix}…</code> · created{" "}
                          {formatTime(k.createTime)}
                        </span>
                      </div>
                      {k.state === ApiKeyState.ACTIVE ? (
                        <Button
                          variant={ButtonVariant.Danger}
                          size={ButtonSize.Small}
                          aria-label={`Revoke API key ${k.name} (${k.keyPrefix})`}
                          onClick={() => setRevoking(k)}
                        >
                          Revoke
                        </Button>
                      ) : null}
                    </li>
                  ))}
                </ul>
              ) : null}
              <MorePagesNotice noun="API keys" pages={pages} />
            </>
          )
        }
      </QueryBoundary>
      <FormDialog
        open={creating}
        onOpenChange={(open) => {
          setCreating(open);
          if (!open) {
            setName("");
            setNameError(undefined);
          }
        }}
        title="Create an API key"
        submitLabel="Create key"
        onSubmit={async (opening) => {
          if (name.trim() === "") {
            setNameError(
              "Enter a name, for example the program that uses the key.",
            );
            return;
          }
          const res = await create.mutateAsync({
            organizationId,
            name: name.trim(),
          });
          // The list refreshes either way (`onSuccess`). A request of a
          // dismissed opening neither closes the form of a later opening nor
          // shows its key; the key stays listed and can be revoked.
          if (!opening.isCurrent()) return;
          setCreating(false);
          setName("");
          setSecret(res.apiKeySecret);
        }}
      >
        <TextField
          label="Name"
          description="Helps you recognize the key later."
          value={name}
          onValueChange={(v) => {
            setName(v);
            setNameError(undefined);
          }}
          error={nameError}
          autoFocus
          maxLength={100}
        />
      </FormDialog>
      <SecretDialog
        title="Your new API key"
        label="API key"
        secret={secret}
        hint="Set it as OWNSAND_API_KEY for the SDK or the CLI."
        onClose={() => setSecret(undefined)}
      />
      <ConfirmDialog
        open={revoking !== undefined}
        onOpenChange={(open) => !open && setRevoking(undefined)}
        title={`Revoke ${revoking?.name ?? "this key"}?`}
        description="Programs that use the key stop working at once. This cannot be undone."
        confirmLabel="Revoke key"
        onConfirm={async () => {
          if (!revoking) return;
          await revoke.mutateAsync({
            organizationId,
            apiKeyId: revoking.apiKeyId,
          });
        }}
      />
    </>
  );
}
