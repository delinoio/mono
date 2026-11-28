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
  toastError,
} from "../components/ui.tsx";
import { formatTime, labels, sessionTitle } from "../format.ts";
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
  const query = useQuery(SessionService.method.getSession, {
    organizationId,
    sessionId,
  });
  const session = query.data?.session;
  // A deleted session, or a session of another organization, shows the
  // not-found notice. The organization shell returns to the picker when the
  // organization itself is gone from the caller's list.
  return (
    <QueryBoundary query={query} loading="Loading the session">
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
  return (
    <dl className="details">
      <Detail term="Agent">{labels.vendor[session.vendor]}</Detail>
      <Detail term="Guest OS">{labels.guestOs[session.guestOs]}</Detail>
      <Detail term="Created">{formatTime(session.createdAt)}</Detail>
      {session.startedAt !== undefined ? (
        <Detail term="Started">{formatTime(session.startedAt)}</Detail>
      ) : null}
      {session.pausedAt !== undefined &&
      session.state === SessionState.PAUSED ? (
        <Detail term="Paused">{formatTime(session.pausedAt)}</Detail>
      ) : null}
      {session.endedAt !== undefined ? (
        <Detail term="Ended">{formatTime(session.endedAt)}</Detail>
      ) : null}
    </dl>
  );
}

/** Other members see the session metadata only, never the transcript. */
function MetadataView({ session }: { session: Session }) {
  const deletion = scheduledDeletionText(session);
  return (
    <>
      <PageHeader
        title={sessionTitle(session)}
        description="This session belongs to another member. Only its owner reads the chat."
        action={<SessionStateBadge session={session} />}
      />
      {deletion !== undefined ? <p className="muted">{deletion}</p> : null}
      <SessionDetails session={session} />
      <p>
        <Link to={orgPath(RouteId.SessionList, session.organizationId)}>
          Back to the sessions
        </Link>
      </p>
    </>
  );
}

function OwnerChat({ session }: { session: Session }) {
  const { organizationId } = useOrg();
  const { navigate } = useRouter();
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
        title={sessionTitle(session)}
        description={`${labels.vendor[session.vendor]} · ${labels.permissionMode[session.permissionMode] ?? ""}`}
        action={
          <div className="header-actions">
            <SessionStateBadge session={session} />
            {!ended ? (
              <Button
                variant={ButtonVariant.Secondary}
                onClick={() => setOptionsOpen(true)}
              >
                Mode and model
              </Button>
            ) : null}
            <Button
              variant={ButtonVariant.Danger}
              onClick={() => setDeleting(true)}
            >
              Delete session
            </Button>
          </div>
        }
      />
      <StateBanner
        session={session}
        onResume={() =>
          resume.mutateAsync({ organizationId, sessionId }).catch(toastError)
        }
      />
      {transcriptState.stream.status === StreamStatus.Reconnecting ? (
        <p className="muted" role="status">
          Reconnecting to the session…
        </p>
      ) : null}
      {transcriptState.stream.status === StreamStatus.Failed ? (
        <ErrorNotice
          title="Live updates stopped."
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
                  .catch(toastError)
        }
      />
      {ended ? (
        <EndedPanel session={session} transcript={transcript} />
      ) : (
        <Composer
          session={session}
          transcript={transcript}
          onSessionChange={onSessionChange}
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
        title={`Delete ${sessionTitle(session)}?`}
        description="SandCrew deletes the sandbox of the session and its transcript permanently. This cannot be undone."
        confirmLabel="Delete session"
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
  switch (session.state) {
    case SessionState.PAUSED: {
      const deletion = scheduledDeletionText(session);
      return (
        <section
          className="state-banner"
          role="status"
          aria-label="Session state"
        >
          {session.resumeWaitExpired ? (
            <>
              <p>
                The session is paused. Its resume waited longer than the runner
                profile allows, because the device had no free capacity.
              </p>
              <Button variant={ButtonVariant.Secondary} onClick={onResume}>
                Resume again
              </Button>
            </>
          ) : (
            <p>
              The session is paused and holds no vCPU or memory. Send a message
              or answer a permission request to resume it.
            </p>
          )}
          {deletion !== undefined ? <p className="muted">{deletion}.</p> : null}
        </section>
      );
    }
    case SessionState.PAUSING:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label="Session state"
        >
          <p>
            The session is pausing. A new message resumes it after the pause.
          </p>
        </section>
      );
    case SessionState.RESUME_QUEUED:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label="Session state"
        >
          <p>
            The resume waits in the SandRun queue for capacity on the device of
            the session.
          </p>
        </section>
      );
    case SessionState.RESUMING:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label="Session state"
        >
          <p>The session is resuming.</p>
        </section>
      );
    case SessionState.DISCONNECTED:
      return (
        <section
          className="state-banner state-banner-warning"
          role="status"
          aria-label="Session state"
        >
          <p>
            The device of the session is offline. The session continues when the
            device is back.
          </p>
        </section>
      );
    case SessionState.QUEUED:
    case SessionState.STARTING:
      return (
        <section
          className="state-banner"
          role="status"
          aria-label="Session state"
        >
          <p>
            {session.state === SessionState.QUEUED
              ? "The session waits in the SandRun queue for a sandbox."
              : "The sandbox is starting the agent."}{" "}
            The agent reads your first message when it is ready.
          </p>
        </section>
      );
    default:
      return null;
  }
}

function TranscriptView({
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
  const end = useRef<HTMLDivElement>(null);
  const count = transcript.items.length;
  useLayoutEffect(() => {
    // Follow new items like a chat.
    end.current?.scrollIntoView?.({ block: "end" });
  }, [count]);
  if (loadError !== undefined && !loaded) {
    return (
      <ErrorNotice
        title="The transcript did not load."
        error={loadError}
        onRetry={onRetry}
      />
    );
  }
  if (!loaded) return <Loading label="Loading the transcript" />;
  return (
    <section className="chat" aria-label="Transcript">
      {count === 0 ? (
        <p className="muted">The transcript is empty so far.</p>
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
}: {
  session: Session;
  transcript: Transcript;
  onSessionChange: (res: { session?: Session }) => void;
}) {
  const { organizationId } = useOrg();
  const [text, setText] = useState("");
  const [error, setError] = useState<string>();
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
      setError("Enter a message.");
      return;
    }
    try {
      await send.mutateAsync({
        organizationId,
        sessionId: session.sessionId,
        clientMessageId: uuidV7(),
        text: message,
      });
      setText("");
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
        label="Message"
        description="Ctrl+Enter or ⌘+Enter sends the message."
        value={text}
        onValueChange={(v: string) => {
          setText(v);
          setError(undefined);
        }}
        onKeyDown={onKeyDown}
        error={error}
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
                .catch(toastError)
            }
          >
            Stop
          </Button>
        ) : null}
        <Button type="submit" disabled={send.isPending}>
          {session.state === SessionState.PAUSED ? "Send and resume" : "Send"}
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
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const [text, setText] = useState("");
  const [error, setError] = useState<string>();
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
      setError("Enter the first message of the new session.");
      return;
    }
    try {
      const res = await restart.mutateAsync({
        organizationId,
        sessionId: session.sessionId,
        firstMessage: message,
      });
      navigate(sessionPath(organizationId, res.session!.sessionId));
    } catch {
      // The form shows the mutation error.
    }
  };

  return (
    <section className="ended" aria-label="Session ended">
      <h2>The session ended</h2>
      <p>{labels.endReason[session.endReason]}.</p>
      {installFailed ? (
        <p>
          The session could not install the agent in the sandbox. Check that the
          device can download the agent, then start a new session.
        </p>
      ) : null}
      <form className="form" onSubmit={submit} noValidate>
        <Textarea
          label="First message of the new session"
          description="The new session gets this transcript as the file .sandcrew/previous-session.md in an empty workspace."
          value={text}
          onValueChange={(v: string) => {
            setText(v);
            setError(undefined);
          }}
          error={error}
          rows={3}
        />
        {restart.error ? <ErrorNotice error={restart.error} /> : null}
        <div className="form-actions">
          <Button type="submit" disabled={restart.isPending}>
            Start new session from transcript
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
        <DialogTitle>Mode and model</DialogTitle>
        <DialogDescription>
          Changes apply to the running agent from its next step.
          {paused ? " Resume the session to change them." : ""}
        </DialogDescription>
        <div className="form">
          <Select
            label="Permissions"
            description="Ask shows each permission request in the chat. Permission prompts are advisory: the sandbox is the security boundary."
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
              {labels.permissionMode[PermissionMode.AUTO_APPROVE]}
            </SelectOption>
            <SelectOption value={String(PermissionMode.ASK)}>
              {labels.permissionMode[PermissionMode.ASK]}
            </SelectOption>
          </Select>
          {modes.length > 0 ? (
            <Select
              label="Mode"
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
            <p className="muted">
              The agent has not advertised modes or models yet.
            </p>
          ) : null}
          {error ? <ErrorNotice error={error} /> : null}
        </div>
        <DialogActions>
          <DialogClose>Done</DialogClose>
        </DialogActions>
      </DialogContent>
    </Dialog>
  );
}
