// SPDX-License-Identifier: Apache-2.0
import { createClient } from "@connectrpc/connect";
import {
  callUnaryMethod,
  createConnectQueryKey,
  useTransport,
} from "@connectrpc/connect-query";
import {
  type GetSessionResponse,
  GetSessionResponseSchema,
  type Session,
  SessionService,
  type TranscriptEvent,
} from "@delino/sandcrew-protos/v1/session_pb";
import { create } from "@bufbuild/protobuf";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { mergeEvents } from "./transcript.ts";
import { usePageVisible, useStream } from "./streams.ts";

/** The page size of ListSessionEvents. */
export const EVENT_PAGE_SIZE = 500;

/**
 * The transcript of one session for its owner: it loads the stored events
 * with ListSessionEvents, then streams new events and session changes with
 * WatchSession from the last sequence it has. The stream reconnects after
 * it ends and stays open only while the page is visible, so the server can
 * suppress notifications for a session that the owner is looking at.
 */
export function useSessionTranscript(
  organizationId: string,
  sessionId: string,
  enabled: boolean,
) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const visible = usePageVisible();
  const [events, setEvents] = useState<readonly TranscriptEvent[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState<unknown>();
  const [loadGeneration, setLoadGeneration] = useState(0);
  const lastSequence = useRef(0n);

  const addEvents = useCallback((added: readonly TranscriptEvent[]) => {
    setEvents((current) => {
      const next = mergeEvents(current, added);
      const last = next.at(-1)?.sequence ?? 0n;
      if (last > lastSequence.current) lastSequence.current = last;
      return next;
    });
  }, []);

  useEffect(() => {
    if (!enabled) return;
    const controller = new AbortController();
    setLoadError(undefined);
    void (async () => {
      try {
        let after = lastSequence.current;
        for (;;) {
          const page = await callUnaryMethod(
            transport,
            SessionService.method.listSessionEvents,
            {
              organizationId,
              sessionId,
              afterSequence: after,
              pageSize: EVENT_PAGE_SIZE,
            },
            { signal: controller.signal },
          );
          addEvents(page.events);
          if (page.nextSequence === 0n || page.nextSequence <= after) break;
          after = page.nextSequence;
        }
        if (!controller.signal.aborted) setLoaded(true);
      } catch (e) {
        if (!controller.signal.aborted) setLoadError(e);
      }
    })();
    return () => controller.abort();
  }, [
    enabled,
    organizationId,
    sessionId,
    transport,
    addEvents,
    loadGeneration,
  ]);

  const sessionKey = useMemo(
    () =>
      createConnectQueryKey({
        schema: SessionService.method.getSession,
        input: { organizationId, sessionId },
        transport,
        cardinality: "finite",
      }),
    [organizationId, sessionId, transport],
  );
  const updateSession = useCallback(
    (session: Session) => {
      queryClient.setQueryData<GetSessionResponse>(sessionKey, () =>
        create(GetSessionResponseSchema, { session }),
      );
    },
    [queryClient, sessionKey],
  );

  const stream = useStream(
    async (signal, onLive) => {
      const client = createClient(SessionService, transport);
      for await (const response of client.watchSession(
        { organizationId, sessionId, afterSequence: lastSequence.current },
        { signal },
      )) {
        const event = response.event;
        switch (event.case) {
          case "transcriptEvent":
            addEvents([event.value]);
            break;
          case "session":
            updateSession(event.value);
            break;
          case "caughtUp":
            onLive();
            break;
          default:
            break;
        }
      }
    },
    {
      enabled: enabled && loaded && visible,
      key: `${organizationId}:${sessionId}`,
    },
  );

  return {
    events,
    loaded,
    loadError,
    retryLoad: () => setLoadGeneration((g) => g + 1),
    stream,
    updateSession,
  };
}
