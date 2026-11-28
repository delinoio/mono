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
import { useEffect, useState } from "react";
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
import { formatTime, labels, sessionTitle } from "../format.ts";
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
 */
function useWatchSessions(
  organizationId: string,
  userId: string | undefined,
  filter: SessionFilter,
) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const visible = usePageVisible();
  return useStream(
    async (signal, onLive) => {
      const client = createClient(SessionService, transport);
      onLive();
      for await (const response of client.watchSessions(
        { organizationId },
        { signal },
      )) {
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
    { enabled: visible, key: `${organizationId}:${filter}` },
  );
}

/** Route `SESSION_LIST`. */
export function SessionListPage() {
  const { organizationId, userId, leaveOnNotFound } = useOrg();
  const { navigate } = useRouter();
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
        title="Sessions"
        description="Agent sessions of the organization. You read the chat of your own sessions; other members' sessions show only their state."
        action={
          <Button
            onClick={() =>
              navigate(orgPath(RouteId.SessionNew, organizationId))
            }
          >
            New session
          </Button>
        }
      />
      <div className="toolbar">
        <SegmentedControl
          aria-label="Sessions to show"
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
            My sessions
          </SegmentedControlItem>
          <SegmentedControlItem value={SessionFilter.All}>
            All sessions
          </SegmentedControlItem>
        </SegmentedControl>
        {watch.status === StreamStatus.Reconnecting ? (
          <span className="muted" role="status">
            Reconnecting to live updates…
          </span>
        ) : null}
      </div>
      <QueryBoundary query={list} loading="Loading sessions">
        {(data) => {
          const sessions = data.pages.flatMap((p) => p.sessions);
          if (sessions.length === 0) {
            return filter === SessionFilter.Mine ? (
              <Empty title="You have no sessions yet">
                Start a session to give a coding agent a task in a sandbox.{" "}
                <Link to={orgPath(RouteId.SessionNew, organizationId)}>
                  Start a session
                </Link>
              </Empty>
            ) : (
              <Empty title="The organization has no sessions yet" />
            );
          }
          return (
            <ul className="list" aria-label="Sessions">
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
  const deletion = scheduledDeletionText(session);
  return (
    <li className="list-row">
      <div className="list-main">
        <Link
          to={sessionPath(session.organizationId, session.sessionId)}
          className="list-title"
        >
          {sessionTitle(session)}
        </Link>
        <span className="muted">
          {labels.vendor[session.vendor]} · {own ? "You" : "Another member"} ·
          created {formatTime(session.createdAt)}
          {session.endedAt !== undefined
            ? ` · ended ${formatTime(session.endedAt)}`
            : ""}
        </span>
        {deletion !== undefined ? (
          <span className="muted">{deletion}</span>
        ) : null}
      </div>
      <SessionStateBadge session={session} />
    </li>
  );
}
