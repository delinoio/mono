// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import {
  PermissionMode,
  SessionEndReason,
  SessionState,
} from "@delino/sandcrew-protos/v1/common_pb";
import {
  type Session,
  SessionService,
} from "@delino/sandcrew-protos/v1/session_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "delino-ui/dialog";
import { Select, SelectOption } from "delino-ui/select";
import { Textarea } from "delino-ui/textarea";
import {
  type FormEvent,
  type KeyboardEvent,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { invalidateServices, uuidV7 } from "../api.ts";
import { ChatItemView } from "../components/chat-items.tsx";
import {
  scheduledDeletionText,
  SessionStateBadge,
} from "../components/session-state.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  Loading,
  PageHeader,
  QueryBoundary,
  useActionSignInNotice,
} from "../components/ui.tsx";
import {
  formatTime,
  guestOsName,
  labels,
  sessionTitle,
  vendorName,
} from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { Link, orgPath, RouteId, sessionPath, useRouter } from "../router.tsx";
import { useSessionTranscript } from "../session-transcript.ts";
import { StreamStatus } from "../streams.ts";
import {
  buildTranscript,
  isTurnRunning,
  type Transcript,
} from "../transcript.ts";

/** Route `SESSION_CHAT`: the chat view of the owner, or the metadata view. */
export function SessionChatPage({ sessionId }: { sessionId: string }) {
  const { organizationId, userId } = useOrg();
  const { t } = useI18n();
  const query = useQuery(SessionService.method.getSession, {
    organizationId,
    sessionId,
  });
  const session = query.data?.session;
  // A deleted session, or a session of another organization, shows the
  // not-found notice. The organization shell returns to the picker when the
  // organization itself is gone from the caller's list.
  return (
    <QueryBoundary query={query} loading={t("chat.loadingSession")}>
      {() =>
        session === undefined ? (
          <ErrorNotice error={new Error("missing session")} />
        ) : session.ownerUserId === userId ? (
          <OwnerChat session={session} />
        ) : (
          <MetadataView session={session} />
        )
      }
    </QueryBoundary>
  );
}

function SessionDetails({ session }: { session: Session }) {
  const { t, lang } = useI18n();
  return (
    <dl className="details">
      <Detail term={t("newSession.agent")}>
        {vendorName(session.vendor, t)}
      </Detail>
      <Detail term={t("chat.details.guestOs")}>
        {guestOsName(session.guestOs, t)}
      </Detail>
      <Detail term={t("chat.details.created")}>
        {formatTime(session.createdAt, lang)}
      </Detail>
      {session.startedAt !== undefined ? (
        <Detail term={t("chat.details.started")}>
          {formatTime(session.startedAt, lang)}
        </Detail>
      ) : null}
      {session.pausedAt !== undefined &&
      session.state === SessionState.PAUSED ? (
        <Detail term={t("chat.details.paused")}>
          {formatTime(session.pausedAt, lang)}
        </Detail>
      ) : null}
      {session.endedAt !== undefined ? (
        <Detail term={t("chat.details.ended")}>
          {formatTime(session.endedAt, lang)}
        </Detail>
      ) : null}
    </dl>
  );
}

/** Other members see the session metadata only, never the transcript. */
function MetadataView({ session }: { session: Session }) {
  const i18n = useI18n();
  const { t } = i18n;
  const deletion = scheduledDeletionText(session, i18n);
  return (
    <>
      <PageHeader
        title={sessionTitle(session, t)}
        description={t("chat.metadataDescription")}
        action={<SessionStateBadge session={session} />}
      />
      {deletion !== undefined ? <p className="muted">{deletion}</p> : null}
      <SessionDetails session={session} />
      <p>
        <Link to={orgPath(RouteId.SessionList, session.organizationId)}>
          {t("chat.backToSessions")}
        </Link>
      </p>
    </>
  );
}

function OwnerChat({ session }: { session: Session }) {
  const { organizationId } = useOrg();
  const { navigate } = useRouter();
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const { sessionId } = session;
  const transcriptState = useSessionTranscript(organizationId, sessionId, true);
  const transcript = useMemo(
    () => buildTranscript(transcriptState.events),
    [transcriptState.events],
  );
  const ended = session.state === SessionState.ENDED;
  const [deleting, setDeleting] = useState(false);
  const [optionsOpen, setOptionsOpen] = useState(false);
  // An expired login fails these actions while the page and its stream stay
  // open: show a lasting sign-in notice instead of a toast.
  const { signInError, reportActionError } = useActionSignInNotice();

  const onSessionChange = (res: { session?: Session }) => {
    if (res.session) transcriptState.updateSession(res.session);
  };
  const answer = useMutation(SessionService.method.answerPermission, {
    onSuccess: onSessionChange,
  });
  const resume = useMutation(SessionService.method.resumeSession, {
    onSuccess: onSessionChange,
  });
  const remove = useMutation(SessionService.method.deleteSession, {
    onSuccess: () => invalidateServices(queryClient, SessionService),
  });

  return (
    <div className="chat-page">
      <PageHeader
        title={sessionTitle(session, t)}
        description={t("chat.headerDescription", {
          vendor: vendorName(session.vendor, t),
          permissions: t(
            labels.permissionMode[session.permissionMode] ?? "enum.unknown",
          ),
        })}
        action={
          <div className="header-actions">
            <SessionStateBadge session={session} />
            {!ended ? (
              <Button
                variant={ButtonVariant.Secondary}
                onClick={() => setOptionsOpen(true)}
              >
                {t("chat.options.title")}
              </Button>
            ) : null}
            <Button
              variant={ButtonVariant.Danger}
              onClick={() => setDeleting(true)}
            >
              {t("chat.delete")}
            </Button>
          </div>
        }
      />
      <StateBanner
        session={session}
        onResume={() =>
          resume
            .mutateAsync({ organizationId, sessionId })
            .catch(reportActionError)
        }
      />
      {signInError ? <ErrorNotice error={signInError} /> : null}
      {transcriptState.stream.status === StreamStatus.Reconnecting ? (
        <p className="muted" role="status">
          {t("chat.reconnecting")}
        </p>
      ) : null}
      {transcriptState.stream.status === StreamStatus.Failed ? (
        <ErrorNotice
          title={t("chat.liveUpdatesStopped")}
          error={transcriptState.stream.error}
          onRetry={transcriptState.stream.retry}
        />
      ) : null}
      <TranscriptView
        transcript={transcript}
        loaded={transcriptState.loaded}
        loadError={transcriptState.loadError}
        onRetry={transcriptState.retryLoad}
        answer={
          ended
            ? undefined
            : (permissionRequestId, optionId) =>
                answer
                  .mutateAsync({
                    organizationId,
                    sessionId,
                    permissionRequestId,
                    optionId,
                  })
                  .catch(reportActionError)
        }
      />
      {ended ? (
        <EndedPanel session={session} transcript={transcript} />
      ) : (
        <Composer
          session={session}
          transcript={transcript}
          onSessionChange={onSessionChange}
          onActionError={reportActionError}
        />
      )}
      {!ended ? (
        <SessionOptionsDialog
          open={optionsOpen}
          onOpenChange={setOptionsOpen}
          session={session}
          transcript={transcript}
          onSessionChange={onSessionChange}
        />
      ) : null}
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={t("chat.deleteTitle", { session: sessionTitle(session, t) })}
        description={t("chat.deleteDescription")}
        confirmLabel={t("chat.delete")}
        onConfirm={async (opening) => {
          await remove.mutateAsync({ organizationId, sessionId });
          if (opening.isCurrent()) {
            navigate(orgPath(RouteId.SessionList, organizationId), {
              replace: true,
            });
          }
        }}
      />
    </div>
  );
}

/** The banner of the states in which the agent cannot answer at once. */
export function StateBanner({
  session,
  onResume,
}: {
  session: Session;
  onResume: () => unknown;
}) {
  const i18n = useI18n();
  const { t } = i18n;
  switch (session.state) {
    case SessionState.PAUSED: {
      const deletion = scheduledDeletionText(session, i18n);
      return (
        <section
          className="state-banner"
          role="status"
          aria-label={t("chat.state.label")}
        >
          {session.resumeWaitExpired ? (
            <>
              <p>{t("chat.state.resumeWaitExpired")}</p>
              <Button variant={ButtonVariant.Secondary} onClick={onResume}>
                {t("chat.state.resumeAgain")}
              </Button>
            </>
          ) : (
            <p>{t("chat.state.paused")}</p>
          )}
          {deletion !== undefined ? (
            <p className="muted">{t("chat.state.deletion", { deletion })}</p>
          ) : null}
        </section>
      );
    }
    case SessionState.PAUSING:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label={t("chat.state.label")}
        >
          <p>{t("chat.state.pausing")}</p>
        </section>
      );
    case SessionState.RESUME_QUEUED:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label={t("chat.state.label")}
        >
          <p>{t("chat.state.resumeQueued")}</p>
        </section>
      );
    case SessionState.RESUMING:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label={t("chat.state.label")}
        >
          <p>{t("chat.state.resuming")}</p>
        </section>
      );
    case SessionState.DISCONNECTED:
      return (
        <section
          className="state-banner state-banner-warning"
          role="status"
          aria-label={t("chat.state.label")}
        >
          <p>{t("chat.state.disconnected")}</p>
        </section>
      );
    case SessionState.QUEUED:
    case SessionState.STARTING:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label={t("chat.state.label")}
        >
          <p>
            {session.state === SessionState.QUEUED
              ? t("chat.state.queued")
              : t("chat.state.starting")}
          </p>
        </section>
      );
    default:
      return null;
  }
}

/**
 * How far below the bottom of the window the end of the transcript can be
 * while the view still follows new output, in pixels. The sticky composer
 * covers the end of the transcript.
 */
const FOLLOW_SLACK_PX = 240;

/** Renders the transcript and follows new output like a chat. */
export function TranscriptView({
  transcript,
  loaded,
  loadError,
  onRetry,
  answer,
}: {
  transcript: Transcript;
  loaded: boolean;
  loadError: unknown;
  onRetry: () => void;
  answer?: (permissionRequestId: string, optionId: string) => Promise<unknown>;
}) {
  const { t } = useI18n();
  const end = useRef<HTMLDivElement>(null);
  const count = transcript.items.length;
  // True while the end of the transcript is in view. Content that is added
  // or that grows (streamed message chunks merge into one item) scrolls the
  // view only then, so a user who scrolled away keeps their position.
  const following = useRef(true);
  useEffect(() => {
    const onScroll = () => {
      const marker = end.current;
      if (!marker) return;
      following.current =
        marker.getBoundingClientRect().top <=
        window.innerHeight + FOLLOW_SLACK_PX;
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);
  useLayoutEffect(() => {
    if (following.current) end.current?.scrollIntoView?.({ block: "end" });
  }, [transcript]);
  if (loadError !== undefined && !loaded) {
    return (
      <ErrorNotice
        title={t("chat.transcript.loadFailed")}
        error={loadError}
        onRetry={onRetry}
      />
    );
  }
  if (!loaded) return <Loading label={t("chat.transcript.loading")} />;
  return (
    <section className="chat" aria-label={t("chat.transcript.label")}>
      {count === 0 ? (
        <p className="muted">{t("chat.transcript.empty")}</p>
      ) : (
        transcript.items.map((item) => (
          <ChatItemView key={item.key} item={item} actions={{ answer }} />
        ))
      )}
      <div ref={end} />
    </section>
  );
}

function Composer({
  session,
  transcript,
  onSessionChange,
  onActionError,
}: {
  session: Session;
  transcript: Transcript;
  onSessionChange: (res: { session?: Session }) => void;
  onActionError: (error: unknown) => void;
}) {
  const { organizationId } = useOrg();
  const { t } = useI18n();
  const [text, setText] = useState("");
  const [error, setError] = useState<MessageKey>();
  // The ID of the last send that did not succeed. A retry of the same text
  // reuses it, so the API deduplicates a message whose response was lost.
  const unconfirmed = useRef<{ text: string; clientMessageId: string }>(
    undefined,
  );
  const send = useMutation(SessionService.method.sendMessage, {
    onSuccess: onSessionChange,
  });
  const cancel = useMutation(SessionService.method.cancelTurn, {
    onSuccess: onSessionChange,
  });
  // A turn that waits for a permission answer is still running: the
  // session is WAITING_FOR_INPUT, and Stop cancels the suspended turn.
  // Paused and ended sessions have no agent to cancel.
  const running =
    isTurnRunning(transcript) &&
    (session.state === SessionState.RUNNING ||
      session.state === SessionState.WAITING_FOR_INPUT);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (send.isPending) return;
    const message = text.trim();
    if (message === "") {
      setError("chat.composer.error");
      return;
    }
    const clientMessageId =
      unconfirmed.current?.text === message
        ? unconfirmed.current.clientMessageId
        : uuidV7();
    unconfirmed.current = { text: message, clientMessageId };
    try {
      await send.mutateAsync({
        organizationId,
        sessionId: session.sessionId,
        clientMessageId,
        text: message,
      });
      unconfirmed.current = undefined;
      // The textarea stays editable while the message sends; keep a newer
      // draft and clear only the text that was sent.
      setText((current) => (current === text ? "" : current));
    } catch {
      // The composer shows the mutation error and keeps the text.
    }
  };
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (
      event.key === "Enter" &&
      (event.metaKey || event.ctrlKey) &&
      !event.nativeEvent.isComposing
    ) {
      event.preventDefault();
      void submit();
    }
  };

  return (
    <form className="composer" onSubmit={submit} noValidate>
      <Textarea
        label={t("chat.composer.label")}
        description={t("chat.composer.help")}
        value={text}
        onValueChange={(v: string) => {
          setText(v);
          setError(undefined);
        }}
        onKeyDown={onKeyDown}
        error={error === undefined ? undefined : t(error)}
        autoFocus
        autoResize
        rows={2}
      />
      {send.error ? <ErrorNotice error={send.error} /> : null}
      <div className="composer-actions">
        {running ? (
          <Button
            type="button"
            variant={ButtonVariant.Secondary}
            disabled={cancel.isPending}
            onClick={() =>
              cancel
                .mutateAsync({
                  organizationId,
                  sessionId: session.sessionId,
                  clientMessageId: uuidV7(),
                })
                .catch(onActionError)
            }
          >
            {t("chat.composer.stop")}
          </Button>
        ) : null}
        <Button type="submit" disabled={send.isPending}>
          {session.state === SessionState.PAUSED
            ? t("chat.composer.sendAndResume")
            : t("chat.composer.send")}
        </Button>
      </div>
    </form>
  );
}

function EndedPanel({
  session,
  transcript,
}: {
  session: Session;
  transcript: Transcript;
}) {
  const { organizationId } = useOrg();
  const { navigate, isCurrentVisit } = useRouter();
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const [text, setText] = useState("");
  const [error, setError] = useState<MessageKey>();
  const restart = useMutation(
    SessionService.method.restartSessionFromTranscript,
    {
      onSuccess: () => invalidateServices(queryClient, SessionService),
    },
  );
  const installFailed =
    session.endReason === SessionEndReason.START_FAILED &&
    transcript.agentInstallFailed;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (restart.isPending) return;
    const message = text.trim();
    if (message === "") {
      setError("chat.ended.error");
      return;
    }
    // The user can leave the chat while the new session starts; the redirect
    // then must not pull them away from the page they went to.
    const stillHere = isCurrentVisit;
    try {
      const res = await restart.mutateAsync({
        organizationId,
        sessionId: session.sessionId,
        firstMessage: message,
      });
      if (stillHere()) {
        navigate(sessionPath(organizationId, res.session!.sessionId));
      }
    } catch {
      // The form shows the mutation error.
    }
  };

  return (
    <section className="ended" aria-label={t("chat.ended.label")}>
      <h2>{t("chat.ended.title")}</h2>
      <p>
        {t("chat.ended.reason", {
          reason: t(labels.endReason[session.endReason] ?? "enum.unknown"),
        })}
      </p>
      {installFailed ? <p>{t("chat.ended.installFailed")}</p> : null}
      <form className="form" onSubmit={submit} noValidate>
        <Textarea
          label={t("chat.ended.firstMessage")}
          description={t("chat.ended.firstMessageHelp")}
          value={text}
          onValueChange={(v: string) => {
            setText(v);
            setError(undefined);
          }}
          error={error === undefined ? undefined : t(error)}
          rows={3}
        />
        {restart.error ? <ErrorNotice error={restart.error} /> : null}
        <div className="form-actions">
          <Button type="submit" disabled={restart.isPending}>
            {t("chat.ended.restart")}
          </Button>
        </div>
      </form>
    </section>
  );
}

/** The select value of the agent's default. */
const NO_VALUE = "";

function SessionOptionsDialog({
  open,
  onOpenChange,
  session,
  transcript,
  onSessionChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  session: Session;
  transcript: Transcript;
  onSessionChange: (res: { session?: Session }) => void;
}) {
  const { organizationId } = useOrg();
  const { t } = useI18n();
  const setMode = useMutation(SessionService.method.setSessionMode, {
    onSuccess: onSessionChange,
  });
  const setOption = useMutation(SessionService.method.setSessionConfigOption, {
    onSuccess: onSessionChange,
  });
  // The latest values come from the transcript; the session metadata has
  // them before the first transcript event of a resumed page.
  const modes =
    transcript.availableModes.length > 0
      ? transcript.availableModes
      : (session.agent?.availableModes ?? []);
  const currentModeId =
    transcript.currentModeId || session.agent?.currentModeId || NO_VALUE;
  const configOptions =
    transcript.configOptions.length > 0
      ? transcript.configOptions
      : (session.agent?.configOptions ?? []);
  const permissionMode = transcript.permissionMode ?? session.permissionMode;
  const paused =
    session.state === SessionState.PAUSED ||
    session.state === SessionState.PAUSING;
  const pending = setMode.isPending || setOption.isPending;
  const error = setMode.error ?? setOption.error;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogTitle>{t("chat.options.title")}</DialogTitle>
        <DialogDescription>
          {paused
            ? t("chat.options.descriptionPaused")
            : t("chat.options.description")}
        </DialogDescription>
        <div className="form">
          <Select
            label={t("chat.options.permissions")}
            description={t("chat.options.permissionsHelp")}
            value={String(permissionMode)}
            disabled={paused || pending}
            onValueChange={(v) => {
              if (v === null || Number(v) === permissionMode) return;
              setMode.mutate({
                organizationId,
                sessionId: session.sessionId,
                clientMessageId: uuidV7(),
                mode: {
                  case: "permissionMode",
                  value: Number(v) as PermissionMode,
                },
              });
            }}
          >
            <SelectOption value={String(PermissionMode.AUTO_APPROVE)}>
              {t(labels.permissionMode[PermissionMode.AUTO_APPROVE])}
            </SelectOption>
            <SelectOption value={String(PermissionMode.ASK)}>
              {t(labels.permissionMode[PermissionMode.ASK])}
            </SelectOption>
          </Select>
          {modes.length > 0 ? (
            <Select
              label={t("newSession.mode")}
              value={currentModeId}
              disabled={paused || pending}
              onValueChange={(v) => {
                if (v === null || v === currentModeId) return;
                setMode.mutate({
                  organizationId,
                  sessionId: session.sessionId,
                  clientMessageId: uuidV7(),
                  mode: { case: "modeId", value: v },
                });
              }}
            >
              {modes.map((m) => (
                <SelectOption key={m.modeId} value={m.modeId}>
                  {m.name}
                </SelectOption>
              ))}
            </Select>
          ) : null}
          {configOptions.map((o) => (
            <Select
              key={o.configOptionId}
              label={o.name}
              value={o.currentValueId}
              disabled={paused || pending}
              onValueChange={(v) => {
                if (v === null || v === o.currentValueId) return;
                setOption.mutate({
                  organizationId,
                  sessionId: session.sessionId,
                  clientMessageId: uuidV7(),
                  configOptionId: o.configOptionId,
                  valueId: v,
                });
              }}
            >
              {o.values.map((value) => (
                <SelectOption key={value.valueId} value={value.valueId}>
                  {value.name}
                </SelectOption>
              ))}
            </Select>
          ))}
          {modes.length === 0 && configOptions.length === 0 ? (
            <p className="muted">{t("chat.options.none")}</p>
          ) : null}
          {error ? <ErrorNotice error={error} /> : null}
        </div>
        <DialogActions>
          <DialogClose>{t("common.done")}</DialogClose>
        </DialogActions>
      </DialogContent>
    </Dialog>
  );
}
