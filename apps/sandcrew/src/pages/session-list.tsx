// SPDX-License-Identifier: Apache-2.0
import type { InfiniteData } from "@tanstack/react-query";
import {
  createConnectQueryKey,
  useInfiniteQuery,
  useTransport,
} from "@connectrpc/connect-query";
import { createClient } from "@connectrpc/connect";
import {
  type ListSessionsResponse,
  type Session,
  SessionService,
} from "@delino/sandcrew-protos/v1/session_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "delino-ui/segmented-control";
import { useEffect, useRef, useState } from "react";
import { invalidateServices, SESSION_PAGE_SIZE } from "../api.ts";
import {
  scheduledDeletionText,
  SessionStateBadge,
} from "../components/session-state.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ListPagination,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import { formatTime, sessionTitle, vendorName } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { Link, orgPath, RouteId, sessionPath, useRouter } from "../router.tsx";
import {
  readSessionFilter,
  SessionFilter,
  writeSessionFilter,
} from "../storage.ts";
import { StreamStatus, usePageVisible, useStream } from "../streams.ts";

type SessionPages = InfiniteData<ListSessionsResponse>;

/** Replaces or removes one session in cached session list pages. */
export function updateSessionPages(
  data: SessionPages | undefined,
  sessionId: string,
  next: Session | undefined,
): { data: SessionPages | undefined; found: boolean } {
  if (data === undefined) return { data, found: false };
  let found = false;
  const pages = data.pages.map((page) => {
    if (!page.sessions.some((s) => s.sessionId === sessionId)) return page;
    found = true;
    return {
      ...page,
      sessions:
        next === undefined
          ? page.sessions.filter((s) => s.sessionId !== sessionId)
          : page.sessions.map((s) => (s.sessionId === sessionId ? next : s)),
    };
  });
  return { data: found ? { ...data, pages } : data, found };
}

/**
 * Keeps the cached session lists of an organization current with
 * WatchSessions while the page is visible. A new session reloads the list.
 * WatchSessions has no replay cursor, so each reopened stream (after a
 * failure, a server rotation or a hidden page) reloads the list once its
 * first response confirms the subscription.
 */
function useWatchSessions(
  organizationId: string,
  userId: string | undefined,
  filter: SessionFilter,
) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const visible = usePageVisible();
  const key = `${organizationId}:${filter}`;
  const watchedKey = useRef<string>(undefined);
  return useStream(
    async (signal, onLive) => {
      const client = createClient(SessionService, transport);
      // The first stream of a list starts with the list load; later streams
      // may have missed changes.
      let reconciled = watchedKey.current !== key;
      watchedKey.current = key;
      let live = false;
      for await (const response of client.watchSessions(
        { organizationId },
        { signal },
      )) {
        // Only a response confirms the subscription; marking the stream
        // live earlier would reset the reconnect backoff on every attempt.
        if (!live) {
          live = true;
          onLive();
        }
        if (!reconciled) {
          reconciled = true;
          void queryClient.invalidateQueries({
            queryKey: createConnectQueryKey({
              schema: SessionService.method.listSessions,
              cardinality: "infinite",
            }),
          });
        }
        const event = response.event;
        if (event.case !== "session" && event.case !== "sessionDeleted") {
          continue;
        }
        const sessionId = event.value.sessionId;
        const next = event.case === "session" ? event.value : undefined;
        let found = false;
        queryClient.setQueriesData<SessionPages>(
          {
            queryKey: createConnectQueryKey({
              schema: SessionService.method.listSessions,
              cardinality: "infinite",
            }),
          },
          (data) => {
            const result = updateSessionPages(data, sessionId, next);
            found ||= result.found;
            return result.data;
          },
        );
        if (
          !found &&
          next !== undefined &&
          (filter === SessionFilter.All || next.ownerUserId === userId)
        ) {
          void invalidateServices(queryClient, SessionService);
        }
      }
    },
    { enabled: visible, key },
  );
}

/** Route `SESSION_LIST`. */
export function SessionListPage() {
  const { organizationId, userId, leaveOnNotFound } = useOrg();
  const { navigate } = useRouter();
  const { t } = useI18n();
  const [filter, setFilter] = useState(readSessionFilter);
  const list = useInfiniteQuery(
    SessionService.method.listSessions,
    {
      organizationId,
      pageSize: SESSION_PAGE_SIZE,
      pageToken: "",
      owner:
        filter === SessionFilter.Mine
          ? { case: "ownedByCaller", value: true }
          : { case: undefined },
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  useEffect(() => {
    leaveOnNotFound(list.error);
  }, [list.error, leaveOnNotFound]);
  const watch = useWatchSessions(organizationId, userId, filter);

  return (
    <>
      <PageHeader
        title={t("nav.sessions")}
        description={t("sessions.description")}
        action={
          <Button
            onClick={() =>
              navigate(orgPath(RouteId.SessionNew, organizationId))
            }
          >
            {t("nav.newSession")}
          </Button>
        }
      />
      <div className="toolbar">
        <SegmentedControl
          aria-label={t("sessions.filter")}
          value={filter}
          onValueChange={(value) => {
            const next =
              value === SessionFilter.All
                ? SessionFilter.All
                : SessionFilter.Mine;
            setFilter(next);
            writeSessionFilter(next);
          }}
        >
          <SegmentedControlItem value={SessionFilter.Mine}>
            {t("sessions.mine")}
          </SegmentedControlItem>
          <SegmentedControlItem value={SessionFilter.All}>
            {t("sessions.all")}
          </SegmentedControlItem>
        </SegmentedControl>
        {watch.status === StreamStatus.Reconnecting ? (
          <span className="muted" role="status">
            {t("sessions.reconnecting")}
          </span>
        ) : null}
      </div>
      <QueryBoundary query={list} loading={t("sessions.loading")}>
        {(data) => {
          const sessions = data.pages.flatMap((p) => p.sessions);
          if (sessions.length === 0) {
            return filter === SessionFilter.Mine ? (
              <Empty title={t("sessions.empty.mine")}>
                {t("sessions.empty.body")}{" "}
                <Link to={orgPath(RouteId.SessionNew, organizationId)}>
                  {t("sessions.empty.start")}
                </Link>
              </Empty>
            ) : (
              <Empty title={t("sessions.empty.all")} />
            );
          }
          return (
            <ul className="list" aria-label={t("nav.sessions")}>
              {sessions.map((s) => (
                <SessionRow
                  key={s.sessionId}
                  session={s}
                  own={s.ownerUserId === userId}
                />
              ))}
            </ul>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
    </>
  );
}

function SessionRow({ session, own }: { session: Session; own: boolean }) {
  const i18n = useI18n();
  const { t, lang } = i18n;
  const deletion = scheduledDeletionText(session, i18n);
  return (
    <li className="list-row">
      <div className="list-main">
        <Link
          to={sessionPath(session.organizationId, session.sessionId)}
          className="list-title"
        >
          {sessionTitle(session, t)}
        </Link>
        <span className="muted">
          {t(
            session.endedAt !== undefined
              ? "sessions.rowEnded"
              : "sessions.row",
            {
              vendor: vendorName(session.vendor, t),
              owner: own ? t("sessions.ownerYou") : t("sessions.ownerOther"),
              created: formatTime(session.createdAt, lang),
              ended: formatTime(session.endedAt, lang),
            },
          )}
        </span>
        {deletion !== undefined ? (
          <span className="muted">{deletion}</span>
        ) : null}
      </div>
      <SessionStateBadge session={session} />
    </li>
  );
}
