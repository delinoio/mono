// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { AuditService } from "@delino/sandrun-protos/v1/audit_pb";
import { useMemo } from "react";
import { PAGE_SIZE } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ListPagination,
  PageHeader,
  QueryBoundary,
  ShortId,
} from "../components/ui.tsx";
import { formatTime, labels, memberName } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { useMembers } from "../members.ts";

/** Route `audit`: audit events of the organization, newest first. */
export function AuditPage() {
  const { lang, t, rich } = useI18n();
  const { organizationId } = useOrg();
  const { members } = useMembers(organizationId);
  const names = useMemo(
    () => new Map(members.map((member) => [member.userId, member.displayName])),
    [members],
  );
  const list = useInfiniteQuery(
    AuditService.method.listAuditEvents,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );

  return (
    <>
      <PageHeader
        title={t("audit.title")}
        description={t("audit.description")}
      />
      <QueryBoundary query={list} loading={t("audit.loading")}>
        {(data) => {
          const events = data.pages.flatMap((p) => p.events);
          return events.length === 0 ? (
            <Empty title={t("audit.empty")} />
          ) : (
            <div
              className="table-wrap"
              role="region"
              aria-label={t("audit.tableRegion")}
              tabIndex={0}
            >
              <table className="table" aria-label={t("audit.title")}>
                <thead>
                  <tr>
                    <th scope="col">{t("audit.column.time")}</th>
                    <th scope="col">{t("audit.column.action")}</th>
                    <th scope="col">{t("audit.column.actor")}</th>
                    <th scope="col">{t("audit.column.target")}</th>
                    <th scope="col">{t("audit.column.subject")}</th>
                  </tr>
                </thead>
                <tbody>
                  {events.map((e) => (
                    <tr key={e.auditEventId}>
                      <td>{formatTime(e.createTime, lang)}</td>
                      <td>{t(labels.auditAction[e.action])}</td>
                      <td>
                        {memberName(names, e.actorUserId)}
                        {e.actorApiKeyId !== "" ? (
                          <span className="muted">
                            {" "}
                            {rich("audit.withApiKey", {
                              key: <ShortId id={e.actorApiKeyId} />,
                            })}
                          </span>
                        ) : null}
                      </td>
                      <td>
                        {t(labels.auditTargetKind[e.targetKind])}{" "}
                        {e.targetId !== "" ? <ShortId id={e.targetId} /> : null}
                      </td>
                      <td>
                        {e.subjectUserId === ""
                          ? "—"
                          : memberName(names, e.subjectUserId)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
    </>
  );
}
