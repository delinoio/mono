// SPDX-License-Identifier: Apache-2.0
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { type ApiKey, ApiKeyService } from "@delino/tide-protos/v1/api_key_pb";
import { ApiKeyState } from "@delino/tide-protos/v1/common_pb";
import { useState } from "react";
import { invalidateServices, MAX_PAGE_SIZE } from "../api.ts";
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

/** Expiry choices of a new key, in days; 0 means no expiry. */
export const EXPIRY_DAYS = [30, 90, 365, 0] as const;
const DAY_MS = 24 * 3600 * 1000;

/**
 * Route `api-keys`: the signed-in user's personal API keys. A key works only
 * for dev sessions; the secret appears once.
 */
export function ApiKeysPage() {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, ApiKeyService);
  const list = useQuery(ApiKeyService.method.listApiKeys, {
    organizationId,
    pageSize: MAX_PAGE_SIZE,
  });
  const create = useMutation(ApiKeyService.method.createApiKey, {
    onSuccess: refresh,
  });
  const revoke = useMutation(ApiKeyService.method.revokeApiKey, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState(String(EXPIRY_DAYS[1]));
  const [nameError, setNameError] = useState<string>();
  const [secret, setSecret] = useState<string>();
  const [revoking, setRevoking] = useState<ApiKey>();
  const keys = list.data?.apiKeys ?? [];

  return (
    <>
      <PageHeader
        title="Personal API keys"
        description="A personal API key lets a coding agent or another program use dev sessions as you, for example with tide mcp. It works only for dev sessions, and only you see your keys."
        action={
          <Button onClick={() => setCreating(true)}>Create API key</Button>
        }
      />
      {list.isPending ? (
        <Loading label="Loading API keys" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={() => void list.refetch()} />
      ) : keys.length === 0 ? (
        <Empty title="No personal API keys">
          Create a key for a coding agent that uses dev sessions.
        </Empty>
      ) : (
        <ul className="list" aria-label="Personal API keys">
          {keys.map((k) => (
            <li key={k.apiKeyId} className="list-row">
              <div className="list-main">
                <span className="list-title">
                  {k.name} · {labels.apiKeyState[k.state]}
                </span>
                <span className="muted">
                  <code>{k.displayPrefix}…</code> · created{" "}
                  {formatTime(k.createdAt)} ·{" "}
                  {k.expiresAt === undefined
                    ? "does not expire"
                    : `expires ${formatTime(k.expiresAt)}`}{" "}
                  · last used{" "}
                  {k.lastUsedAt === undefined
                    ? "never"
                    : formatTime(k.lastUsedAt)}
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
        title="Create a personal API key"
        description="The key authenticates as you, but only on dev session calls."
        submitLabel="Create key"
        onSubmit={async () => {
          const trimmed = name.trim();
          if (trimmed === "" || trimmed.length > 64) {
            setNameError(
              "Enter a name of 1 to 64 characters, for example the agent that uses the key.",
            );
            return;
          }
          const days = Number(expiry);
          const res = await create.mutateAsync({
            organizationId,
            name: trimmed,
            expiresAt:
              days === 0
                ? undefined
                : timestampFromMs(Date.now() + days * DAY_MS),
          });
          setCreating(false);
          setName("");
          setSecret(res.secret);
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
          maxLength={64}
          autoComplete="off"
        />
        <Select
          label="Expires after"
          value={expiry}
          onValueChange={(v) => v && setExpiry(v)}
        >
          {EXPIRY_DAYS.map((d) => (
            <SelectOption key={d} value={String(d)}>
              {d === 0 ? "Never" : `${d} days`}
            </SelectOption>
          ))}
        </Select>
      </FormDialog>
      <SecretDialog
        title="Your new personal API key"
        label="API key"
        secret={secret}
        hint="It works only for dev sessions. Set it as TIDE_API_KEY for the tide CLI or tide mcp."
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
