// SPDX-License-Identifier: Apache-2.0
import {
  createInfiniteQueryOptions,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { EnrollmentTokenState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type EnrollmentToken,
  DeviceService,
} from "@delino/ownsand-protos/v1/device_pb";
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
  ShortId,
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
import { repeatedPrefixes } from "../revocation.ts";
import { enumText } from "../text.ts";

/** Token lifetimes that the console offers; the maximum is 24 hours. */
const TTL_HOURS = [1, 6, 12, 24] as const;

/** Route `enrollment-tokens`: create, list and revoke enrollment tokens. */
export function EnrollmentTokensPage() {
  const { organizationId } = useOrg();
  const i18n = useI18n();
  const { t, lang } = i18n;
  const queryClient = useQueryClient();
  const transport = useTransport();
  const refresh = () => invalidateServices(queryClient, DeviceService);
  // The control plane lists tokens in ascending ID order, so a new token is on
  // the last page. Read every page, or it could not be revoked from here.
  const listOptions = (listOrganizationId: string) =>
    createInfiniteQueryOptions(
      DeviceService.method.listEnrollmentTokens,
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
  const tokens = useMemo(
    () =>
      uniqueRows(
        list.data?.pages.map((page) => page.tokens) ?? [],
        (t) => t.enrollmentTokenId,
      ),
    [list.data],
  );
  const collisions = useMemo(
    () =>
      repeatedPrefixes(
        tokens
          .filter((token) => token.state === EnrollmentTokenState.PENDING)
          .map((token) => token.tokenPrefix),
      ),
    [tokens],
  );
  const create = useMutation(DeviceService.method.createEnrollmentToken, {
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
  const revoke = useMutation(DeviceService.method.revokeEnrollmentToken, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [ttl, setTtl] = useState("1");
  const [token, setToken] = useState<string>();
  const [revoking, setRevoking] = useState<EnrollmentToken>();
  const revokingId =
    revoking && collisions.has(revoking.tokenPrefix)
      ? ` (${revoking.enrollmentTokenId})`
      : "";

  if (isTerminalError(pages.error) && revoking !== undefined) {
    setRevoking(undefined);
  }

  return (
    <>
      <PageHeader
        title={t("enrollmentTokens.title")}
        description={t("enrollmentTokens.description")}
        action={
          <Button onClick={() => setCreating(true)}>
            {t("enrollmentTokens.create")}
          </Button>
        }
      />
      <QueryBoundary
        query={boundaryOf(list, pages)}
        loading={t("enrollmentTokens.loading")}
      >
        {() =>
          tokens.length === 0 && !pages.isIncomplete ? (
            <Empty title={t("enrollmentTokens.empty")} />
          ) : (
            <>
              {tokens.length > 0 ? (
                <ul className="list" aria-label={t("enrollmentTokens.list")}>
                  {tokens.map((t) => {
                    const pending = t.state === EnrollmentTokenState.PENDING;
                    const showId = pending && collisions.has(t.tokenPrefix);
                    return (
                      <li key={t.enrollmentTokenId} className="list-row">
                        <div className="list-main">
                          <span className="list-title">
                            {i18n.rich("enrollmentTokens.rowTitle", {
                              prefix: <code>{t.tokenPrefix}…</code>,
                              state: enumText(i18n, labels.tokenState, t.state),
                            })}
                          </span>
                          <span className="muted">
                            {i18n.t("enrollmentTokens.rowTimes", {
                              created: formatTime(t.createTime, lang),
                              expires: formatTime(t.expireTime, lang),
                            })}
                            {t.deviceId ? (
                              <>
                                {" "}
                                {i18n.rich("enrollmentTokens.rowDevice", {
                                  device: <ShortId id={t.deviceId} />,
                                })}
                              </>
                            ) : null}
                          </span>
                          {showId ? (
                            <span className="muted list-record-id">
                              {i18n.rich("invitations.recordId", {
                                id: <code>{t.enrollmentTokenId}</code>,
                              })}
                            </span>
                          ) : null}
                        </div>
                        {pending ? (
                          <Button
                            variant={ButtonVariant.Secondary}
                            size={ButtonSize.Small}
                            aria-label={
                              showId
                                ? i18n.t("enrollmentTokens.revokeNamedWithId", {
                                    prefix: t.tokenPrefix,
                                    id: t.enrollmentTokenId,
                                  })
                                : i18n.t("enrollmentTokens.revokeNamed", {
                                    prefix: t.tokenPrefix,
                                  })
                            }
                            onClick={() => setRevoking(t)}
                          >
                            {i18n.t("enrollmentTokens.revoke")}
                          </Button>
                        ) : null}
                      </li>
                    );
                  })}
                </ul>
              ) : null}
              <MorePagesNotice noun={ListNoun.EnrollmentTokens} pages={pages} />
            </>
          )
        }
      </QueryBoundary>
      <ConfirmDialog
        open={revoking !== undefined}
        onOpenChange={(open) => !open && setRevoking(undefined)}
        title={t("enrollmentTokens.revokeDialog.title", {
          token: revoking ? `${revoking.tokenPrefix}…${revokingId}` : "",
        })}
        description={t("enrollmentTokens.revokeDialog.description")}
        confirmLabel={t("enrollmentTokens.revokeDialog.confirm")}
        onConfirm={async () => {
          if (!revoking) return;
          await revoke.mutateAsync({
            organizationId,
            enrollmentTokenId: revoking.enrollmentTokenId,
          });
          toast.success(t("enrollmentTokens.revoked"));
        }}
      />
      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title={t("enrollmentTokens.createDialog.title")}
        submitLabel={t("enrollmentTokens.create")}
        onSubmit={async (opening) => {
          const res = await create.mutateAsync({
            organizationId,
            ttl: { seconds: BigInt(Number(ttl) * 3600) },
          });
          // The list refreshes either way (`onSuccess`). A request of a
          // dismissed opening neither closes the form of a later opening nor
          // shows its token; the token stays listed and can be revoked.
          if (!opening.isCurrent()) return;
          setCreating(false);
          setToken(res.enrollmentToken);
        }}
      >
        <Select
          label={t("enrollmentTokens.createDialog.expiresAfter")}
          value={ttl}
          onValueChange={(v) => v && setTtl(v)}
        >
          {TTL_HOURS.map((h) => (
            <SelectOption key={h} value={String(h)}>
              {i18n.plural("enrollmentTokens.createDialog.hours", h)}
            </SelectOption>
          ))}
        </Select>
      </FormDialog>
      <SecretDialog
        title={t("enrollmentTokens.secret.title")}
        label={t("enrollmentTokens.secret.label")}
        secret={token}
        hint={t("enrollmentTokens.secret.hint", {
          command: "ownsand-agent enroll",
        })}
        onClose={() => setToken(undefined)}
      />
    </>
  );
}
