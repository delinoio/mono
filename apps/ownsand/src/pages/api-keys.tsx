// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { ApiKeyState } from "ownsand-sdk/gen/common_pb";
import { type ApiKey, ApiKeyService } from "ownsand-sdk/gen/api_key_pb";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  FormDialog,
  Loading,
  PageHeader,
  SecretDialog,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";

/** Route `api-keys`: create, list and revoke organization API keys. */
export function ApiKeysPage() {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, ApiKeyService);
  const list = useQuery(ApiKeyService.method.listApiKeys, {
    organizationId,
    pageSize: 200,
  });
  const create = useMutation(ApiKeyService.method.createApiKey, {
    onSuccess: refresh,
  });
  const revoke = useMutation(ApiKeyService.method.revokeApiKey, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [nameError, setNameError] = useState<string>();
  const [secret, setSecret] = useState<string>();
  const [revoking, setRevoking] = useState<ApiKey>();

  return (
    <>
      <PageHeader
        title="API keys"
        description="Programs such as CI jobs and AI agents use API keys to manage sandboxes. A key cannot read members."
        action={
          <Button onClick={() => setCreating(true)}>Create API key</Button>
        }
      />
      {list.isPending ? (
        <Loading label="Loading API keys" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={() => void list.refetch()} />
      ) : list.data.apiKeys.length === 0 ? (
        <Empty title="No API keys" />
      ) : (
        <ul className="list" aria-label="API keys">
          {list.data.apiKeys.map((k) => (
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
                  onClick={() => setRevoking(k)}
                >
                  Revoke
                </Button>
              ) : null}
            </li>
          ))}
        </ul>
      )}
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
        onSubmit={async () => {
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
        onConfirm={() =>
          revoke.mutateAsync({ organizationId, apiKeyId: revoking!.apiKeyId })
        }
      />
    </>
  );
}
