// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery, useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { Select, SelectOption } from "delino-ui/select";
import {
  type DevSession,
  DevSessionService,
} from "@delino/sandrun-protos/v1/dev_session_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { useMemo, useState } from "react";
import { invalidateServices, PAGE_SIZE } from "../api.ts";
import { SetupEmpty } from "../components/setup.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  ListPagination,
  PageHeader,
  QueryBoundary,
  ShortId,
  StateBadge,
} from "../components/ui.tsx";
import {
  formatTime,
  guestOsName,
  labels,
  memberName,
  TERMINAL_REQUEST_STATES,
} from "../format.ts";
import { useI18n } from "../i18n.ts";
import { useMembers } from "../members.ts";
import { canDeleteDevSession } from "../roles.ts";
import { Link, orgPath, RouteId } from "../router.tsx";
import { requestStateTone } from "./requests.tsx";

/** Owner filter of the dev session list. */
export enum OwnerFilter {
  Everyone = "everyone",
  Mine = "mine",
}

/**
 * Route `dev-sessions`: dev session metadata of the organization. It shows
 * no command, output or file content and runs no command; commands and
 * file transfers are available only through the CLI, the MCP server and
 * the SDKs.
 */
export function DevSessionsPage() {
  const { lang, t, rich } = useI18n();
  const { organizationId, role, userId } = useOrg();
  const queryClient = useQueryClient();
  const [owner, setOwner] = useState<OwnerFilter>(OwnerFilter.Everyone);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [deleting, setDeleting] = useState<DevSession>();
  const { members } = useMembers(organizationId);
  const names = useMemo(
    () => new Map(members.map((member) => [member.userId, member.displayName])),
    [members],
  );
  const list = useInfiniteQuery(
    DevSessionService.method.listDevSessions,
    {
      organizationId,
      pageSize: PAGE_SIZE,
      pageToken: "",
      owner:
        owner === OwnerFilter.Mine
          ? { case: "ownedByCaller", value: true }
          : { case: undefined },
      includeEnded,
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const remove = useMutation(DevSessionService.method.deleteDevSession, {
    onSuccess: () =>
      invalidateServices(queryClient, DevSessionService, WorkService),
  });

  return (
    <>
      <PageHeader
        title={t("devSessions.title")}
        description={t("devSessions.description")}
      />
      <div className="filters">
        <Select
          label={t("devSessions.filter.owner")}
          value={owner}
          onValueChange={(v) =>
            setOwner((v as OwnerFilter) ?? OwnerFilter.Everyone)
          }
        >
          <SelectOption value={OwnerFilter.Everyone}>
            {t("devSessions.filter.everyone")}
          </SelectOption>
          <SelectOption value={OwnerFilter.Mine}>
            {t("devSessions.filter.mine")}
          </SelectOption>
        </Select>
        <Checkbox
          label={t("devSessions.filter.showEnded")}
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      <QueryBoundary query={list} loading={t("devSessions.loading")}>
        {(data) => {
          const sessions = data.pages.flatMap((p) => p.devSessions);
          return sessions.length === 0 ? (
            <SetupEmpty
              organizationId={organizationId}
              title={t("devSessions.empty.title")}
            >
              {rich("devSessions.empty.description", {
                command: <code>sandrun session create</code>,
              })}{" "}
              {rich("devSessions.empty.apiKey", {
                link: (
                  <Link to={orgPath(RouteId.ApiKeys, organizationId)}>
                    {t("devSessions.empty.apiKeyLink")}
                  </Link>
                ),
              })}
            </SetupEmpty>
          ) : (
            <div
              className="table-wrap"
              role="region"
              aria-label={t("devSessions.tableRegion")}
              tabIndex={0}
            >
              <table className="table" aria-label={t("devSessions.title")}>
                <thead>
                  <tr>
                    <th scope="col">{t("devSessions.column.session")}</th>
                    <th scope="col">{t("devSessions.column.owner")}</th>
                    <th scope="col">{t("common.column.profile")}</th>
                    <th scope="col">{t("common.column.guestOs")}</th>
                    <th scope="col">{t("common.column.state")}</th>
                    <th scope="col">{t("common.column.endReason")}</th>
                    <th scope="col">{t("common.column.queued")}</th>
                    <th scope="col">{t("common.column.actions")}</th>
                  </tr>
                </thead>
                <tbody>
                  {sessions.map((s) => {
                    const ended = TERMINAL_REQUEST_STATES.has(s.state);
                    return (
                      <tr key={s.devSessionId}>
                        <td>
                          {s.label !== "" ? `${s.label} ` : null}
                          <ShortId id={s.devSessionId} />
                        </td>
                        <td>
                          {memberName(names, s.ownerUserId)}
                          {s.ownerUserId === userId ? (
                            <span className="muted"> {t("common.you")}</span>
                          ) : null}
                        </td>
                        <td>
                          {s.runnerProfileName || (
                            <ShortId id={s.runnerProfileId} />
                          )}
                        </td>
                        <td>{guestOsName(s.guestOs, t)}</td>
                        <td>
                          <StateBadge
                            text={t(labels.requestState[s.state])}
                            tone={requestStateTone(s.state)}
                          />
                        </td>
                        <td>
                          {ended
                            ? t(labels.requestEndReason[s.endReason])
                            : "—"}
                        </td>
                        <td>{formatTime(s.queuedAt, lang)}</td>
                        <td>
                          {!ended && canDeleteDevSession(role, userId, s) ? (
                            <Button
                              variant={ButtonVariant.Danger}
                              size={ButtonSize.Small}
                              onClick={() => setDeleting(s)}
                            >
                              {t("common.delete")}
                            </Button>
                          ) : null}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={t("devSessions.deleteDialog.title")}
        description={t("devSessions.deleteDialog.description")}
        confirmLabel={t("devSessions.deleteDialog.confirm")}
        onConfirm={() =>
          remove.mutateAsync({
            organizationId,
            devSessionId: deleting!.devSessionId,
          })
        }
      />
    </>
  );
}
