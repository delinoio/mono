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
  ListNoun,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  isTerminalError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import {
  FOLLOW_ALL_PAGES_OPTIONS,
  boundaryOf,
  cancelUncachedList,
  uniqueRows,
  useFollowAllPages,
} from "../pagination.ts";
import { enumText } from "../text.ts";

/** Route `api-keys`: create, list and revoke organization API keys. */
export function ApiKeysPage() {
  const { organizationId } = useOrg();
  const i18n = useI18n();
  const { t, lang } = i18n;
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
  const { apiKeys, collidingIds } = useMemo(() => {
    const apiKeys = uniqueRows(
      list.data?.pages.map((page) => page.apiKeys) ?? [],
      (key) => key.apiKeyId,
    );
    const firstIdByNameAndPrefix = new Map<string, string>();
    const collidingIds = new Set<string>();
    for (const key of apiKeys) {
      if (key.state !== ApiKeyState.ACTIVE) continue;
      const nameAndPrefix = JSON.stringify([key.name, key.keyPrefix]);
      const firstId = firstIdByNameAndPrefix.get(nameAndPrefix);
      if (firstId === undefined) {
        firstIdByNameAndPrefix.set(nameAndPrefix, key.apiKeyId);
      } else {
        collidingIds.add(firstId);
        collidingIds.add(key.apiKeyId);
      }
    }
    return { apiKeys, collidingIds };
  }, [list.data]);
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
        title={t("apiKeys.title")}
        description={t("apiKeys.description")}
        action={
          <Button onClick={() => setCreating(true)}>
            {t("apiKeys.create")}
          </Button>
        }
      />
      <QueryBoundary
        query={boundaryOf(list, pages)}
        loading={t("apiKeys.loading")}
      >
        {() =>
          apiKeys.length === 0 && !pages.isIncomplete ? (
            <Empty title={t("apiKeys.empty")} />
          ) : (
            <>
              {apiKeys.length > 0 ? (
                <ul className="list" aria-label={t("apiKeys.list")}>
                  {apiKeys.map((k) => {
                    const showId = collidingIds.has(k.apiKeyId);
                    return (
                      <li key={k.apiKeyId} className="list-row">
                        <div className="list-main">
                          <span className="list-title">
                            {t("apiKeys.rowTitle", {
                              name: k.name,
                              state: enumText(
                                i18n,
                                labels.apiKeyState,
                                k.state,
                              ),
                            })}
                          </span>
                          <span className="muted">
                            {i18n.rich("apiKeys.rowDetails", {
                              prefix: <code>{k.keyPrefix}…</code>,
                              time: formatTime(k.createTime, lang),
                            })}
                          </span>
                          {showId ? (
                            <span className="muted api-key-metadata">
                              {i18n.rich("apiKeys.keyId", {
                                id: <code>{k.apiKeyId}</code>,
                              })}
                            </span>
                          ) : null}
                        </div>
                        {k.state === ApiKeyState.ACTIVE ? (
                          <Button
                            variant={ButtonVariant.Danger}
                            size={ButtonSize.Small}
                            aria-label={
                              showId
                                ? t("apiKeys.revokeNamedWithId", {
                                    name: k.name,
                                    prefix: k.keyPrefix,
                                    id: k.apiKeyId,
                                  })
                                : t("apiKeys.revokeNamed", {
                                    name: k.name,
                                    prefix: k.keyPrefix,
                                  })
                            }
                            onClick={() => setRevoking(k)}
                          >
                            {t("apiKeys.revoke")}
                          </Button>
                        ) : null}
                      </li>
                    );
                  })}
                </ul>
              ) : null}
              <MorePagesNotice noun={ListNoun.ApiKeys} pages={pages} />
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
        title={t("apiKeys.createDialog.title")}
        submitLabel={t("apiKeys.createDialog.submit")}
        onSubmit={async (opening) => {
          if (name.trim() === "") {
            setNameError(t("apiKeys.createDialog.nameRequired"));
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
          label={t("apiKeys.createDialog.name")}
          description={t("apiKeys.createDialog.nameDescription")}
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
        title={t("apiKeys.secret.title")}
        label={t("apiKeys.secret.label")}
        secret={secret}
        hint={t("apiKeys.secret.hint", { variable: "OWNSAND_API_KEY" })}
        onClose={() => setSecret(undefined)}
      />
      <ConfirmDialog
        open={revoking !== undefined}
        onOpenChange={(open) => !open && setRevoking(undefined)}
        title={
          revoking
            ? t("apiKeys.revokeDialog.title", { name: revoking.name })
            : t("apiKeys.revokeDialog.titleUnnamed")
        }
        description={
          <>
            {revoking ? (
              <span className="api-key-metadata">
                {i18n.rich("apiKeys.prefix", {
                  prefix: <code>{revoking.keyPrefix}</code>,
                })}
                {collidingIds.has(revoking.apiKeyId) ? (
                  <>
                    <br />
                    {i18n.rich("apiKeys.keyId", {
                      id: <code>{revoking.apiKeyId}</code>,
                    })}
                  </>
                ) : null}
                <br />
              </span>
            ) : null}
            {t("apiKeys.revokeDialog.description")}
          </>
        }
        confirmLabel={t("apiKeys.revokeDialog.confirm")}
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
