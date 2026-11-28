// SPDX-License-Identifier: Apache-2.0
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import type { Transport } from "@connectrpc/connect";
import {
  createInfiniteQueryOptions,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import {
  type ApiKey,
  ApiKeyService,
} from "@delino/sandrun-protos/v1/api_key_pb";
import { ApiKeyState } from "@delino/sandrun-protos/v1/common_pb";
import { useState } from "react";
import { invalidateServices, PAGE_SIZE } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  FormDialog,
  ListPagination,
  PageHeader,
  QueryBoundary,
  SecretDialog,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { API_KEY_NAME_LIMIT, isNameLengthValid } from "../name.ts";

/** Expiry choices of a new key, in days; 0 means no expiry. */
export const EXPIRY_DAYS = [30, 90, 365, 0] as const;
const DAY_MS = 24 * 3600 * 1000;

function apiKeyListOptions(
  organizationId: string | undefined,
  transport: Transport,
) {
  return createInfiniteQueryOptions(
    ApiKeyService.method.listApiKeys,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    {
      transport,
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
}

/**
 * Route `api-keys`: the signed-in user's personal API keys. A key works only
 * for dev sessions; the secret appears once.
 */
export function ApiKeysPage() {
  const { lang, t, plural } = useI18n();
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const transport = useTransport();
  const refresh = () => invalidateServices(queryClient, ApiKeyService);
  const list = useInfiniteQuery(apiKeyListOptions(organizationId, transport));
  const create = useMutation(ApiKeyService.method.createApiKey, {
    onSuccess: (_, request) => {
      // Invalidation alone can reuse the first fetch when it has no cached data.
      const { queryKey } = apiKeyListOptions(request.organizationId, transport);
      void queryClient
        .cancelQueries({
          queryKey,
          exact: true,
          fetchStatus: "fetching",
          predicate: (query) => query.state.data === undefined,
        })
        .then(refresh);
    },
  });
  const revoke = useMutation(ApiKeyService.method.revokeApiKey, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState(String(EXPIRY_DAYS[1]));
  const [nameError, setNameError] = useState<MessageKey>();
  const [secret, setSecret] = useState<string>();
  const [revoking, setRevoking] = useState<ApiKey>();

  return (
    <>
      <PageHeader
        title={t("apiKeys.title")}
        description={t("apiKeys.description", { command: "sandrun mcp" })}
        action={
          <Button onClick={() => setCreating(true)}>
            {t("apiKeys.create")}
          </Button>
        }
      />
      <QueryBoundary query={list} loading={t("apiKeys.loading")}>
        {(data) => {
          const keys = data.pages.flatMap((p) => p.apiKeys);
          return keys.length === 0 ? (
            <Empty title={t("apiKeys.empty.title")}>
              {t("apiKeys.empty.description")}
            </Empty>
          ) : (
            <ul className="list" aria-label={t("apiKeys.title")}>
              {keys.map((k) => (
                <li key={k.apiKeyId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {k.name} · {t(labels.apiKeyState[k.state])}
                    </span>
                    <span className="muted">
                      <code>{k.displayPrefix}…</code> ·{" "}
                      {[
                        t("apiKeys.created", {
                          time: formatTime(k.createdAt, lang),
                        }),
                        k.expiresAt === undefined
                          ? t("apiKeys.noExpiry")
                          : t("apiKeys.expires", {
                              time: formatTime(k.expiresAt, lang),
                            }),
                        k.lastUsedAt === undefined
                          ? t("apiKeys.neverUsed")
                          : t("apiKeys.lastUsed", {
                              time: formatTime(k.lastUsedAt, lang),
                            }),
                      ].join(" · ")}
                    </span>
                  </div>
                  {k.state === ApiKeyState.ACTIVE ? (
                    <Button
                      variant={ButtonVariant.Danger}
                      size={ButtonSize.Small}
                      onClick={() => setRevoking(k)}
                    >
                      {t("apiKeys.revoke")}
                    </Button>
                  ) : null}
                </li>
              ))}
            </ul>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
      <FormDialog
        open={creating}
        onOpenChange={(open) => {
          setCreating(open);
          if (!open) {
            setName("");
            setNameError(undefined);
          }
        }}
        title={t("apiKeys.createDialog.title")}
        description={t("apiKeys.createDialog.description")}
        submitLabel={t("apiKeys.createDialog.submit")}
        onSubmit={async (opening) => {
          const trimmed = name.trim();
          if (!isNameLengthValid(trimmed, API_KEY_NAME_LIMIT)) {
            setNameError("apiKeys.createDialog.nameInvalid");
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
          if (!opening.isCurrent()) return;
          setCreating(false);
          setName("");
          setSecret(res.secret);
        }}
      >
        <TextField
          label={t("apiKeys.createDialog.nameLabel")}
          description={t("apiKeys.createDialog.nameDescription")}
          value={name}
          onValueChange={(v) => {
            setName(v);
            setNameError(undefined);
          }}
          error={nameError && t(nameError, { max: API_KEY_NAME_LIMIT })}
          autoFocus
          autoComplete="off"
        />
        <Select
          label={t("apiKeys.createDialog.expiry")}
          value={expiry}
          onValueChange={(v) => v && setExpiry(v)}
        >
          {EXPIRY_DAYS.map((d) => (
            <SelectOption key={d} value={String(d)}>
              {d === 0
                ? t("apiKeys.createDialog.expiryNever")
                : plural("apiKeys.createDialog.expiryDays", d)}
            </SelectOption>
          ))}
        </Select>
      </FormDialog>
      <SecretDialog
        title={t("apiKeys.secretDialog.title")}
        label={t("apiKeys.secretDialog.label")}
        secret={secret}
        hint={t("apiKeys.secretDialog.hint", {
          variable: "SANDRUN_API_KEY",
          command: "sandrun mcp",
        })}
        onClose={() => setSecret(undefined)}
      />
      <ConfirmDialog
        open={revoking !== undefined}
        onOpenChange={(open) => !open && setRevoking(undefined)}
        title={
          revoking === undefined
            ? t("apiKeys.revokeDialog.titleUnnamed")
            : t("apiKeys.revokeDialog.title", { name: revoking.name })
        }
        description={t("apiKeys.revokeDialog.description")}
        confirmLabel={t("apiKeys.revokeDialog.confirm")}
        onConfirm={() =>
          revoke.mutateAsync({ organizationId, apiKeyId: revoking!.apiKeyId })
        }
      />
    </>
  );
}
