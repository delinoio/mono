// SPDX-License-Identifier: Apache-2.0
import { Badge, BadgeVariant } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import {
  AlertDialog,
  AlertDialogActions,
  AlertDialogClose,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogTitle,
} from "delino-ui/alert-dialog";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "delino-ui/dialog";
import { Skeleton } from "delino-ui/skeleton";
import { toast } from "delino-ui/toast";
import { Code, ConnectError } from "@connectrpc/connect";
import { DeviceState, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type FormEvent,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";
import { billingReasonOf } from "../api.ts";
import { useAuth } from "../auth.tsx";
import { ACCOUNTS_URL } from "../config.ts";
import { errorMessage, labels } from "../format.ts";
import type { FollowedPages } from "../pagination.ts";

export function PageHeader({
  title,
  description,
  action,
}: {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <header className="page-header">
      <div>
        <h1>{title}</h1>
        {description ? <p className="muted">{description}</p> : null}
      </div>
      {action ? <div className="page-header-action">{action}</div> : null}
    </header>
  );
}

/**
 * Shows a failed action in a toast. A billing error also gets a link
 * action to the billing site, because toast text cannot hold a link.
 */
export function toastError(error: unknown): void {
  if (billingReasonOf(error) !== undefined) {
    toast.error(errorMessage(error), {
      action: { label: "Open billing", href: ACCOUNTS_URL },
    });
    return;
  }
  toast.error(errorMessage(error));
}

/** A loading placeholder for a list. */
export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="loading" role="status" aria-label={label}>
      <Skeleton height="56px" />
      <Skeleton height="56px" />
      <Skeleton height="56px" />
    </div>
  );
}

/**
 * Text in which every occurrence of the billing site address is a link,
 * for server messages such as billing errors.
 */
export function TextWithAccountsLink({ text }: { text: string }) {
  const parts = text.split(ACCOUNTS_URL);
  return (
    <>
      {parts.map((part, i) => (
        <span key={i}>
          {i > 0 ? (
            <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
              {ACCOUNTS_URL}
            </a>
          ) : null}
          {part}
        </span>
      ))}
    </>
  );
}

/**
 * The sign-in button of an `unauthenticated` notice. It reads the auth context
 * here and not in `ErrorNotice`, so that a notice for any other error (for
 * example the callback page's, which a test or a caller may mount without an
 * auth provider) does not need one.
 */
function SignInButton({ onSignIn }: { onSignIn?: () => void | Promise<void> }) {
  const auth = useAuth();
  return (
    <>
      <Button
        variant={ButtonVariant.Secondary}
        onClick={() => {
          void (onSignIn ?? auth.signIn)();
        }}
      >
        Sign in with Delino
      </Button>
      <SignInFailureNotice />
    </>
  );
}

/**
 * The alert for a sign-in that failed before the redirect started. The text
 * is fixed: the SDK error can carry request details, and the user can only
 * retry. It renders nothing while the latest attempt did not fail, and the
 * next attempt clears it.
 */
export function SignInFailureNotice() {
  const auth = useAuth();
  return auth.signInFailed ? (
    <p role="alert">
      Sign-in did not start. Check your connection and try again.
    </p>
  ) : null;
}

/**
 * A failed request, with a retry button. A billing error shows the server
 * message with its link and never offers a retry, because it is terminal.
 * An `unauthenticated` error, from the server or from a token request that
 * failed, cannot succeed on a retry: the notice offers the sign-in instead.
 * The Logto SDK keeps reporting the user as signed in after a failed token
 * refresh, so the signed-out view never shows. `onSignIn` replaces the
 * default `auth.signIn()` for a page that must prepare the redirect first.
 * A `not_found` or `invalid_argument` error never offers a retry either: the
 * same request fails the same way, so the button would be futile. `children`
 * (for example a link out of the page) render after the buttons.
 */
export function ErrorNotice({
  error,
  onRetry,
  onSignIn,
  children,
}: {
  error: unknown;
  onRetry?: () => void;
  onSignIn?: () => void | Promise<void>;
  children?: ReactNode;
}) {
  const billing = billingReasonOf(error) !== undefined;
  const retryFutile =
    error instanceof ConnectError &&
    (error.code === Code.NotFound || error.code === Code.InvalidArgument);
  const unauthenticated =
    error instanceof ConnectError && error.code === Code.Unauthenticated;
  return (
    <div className="notice notice-error" role="alert">
      <p>
        <TextWithAccountsLink text={errorMessage(error)} />
      </p>
      {unauthenticated ? (
        <SignInButton onSignIn={onSignIn} />
      ) : onRetry && !billing && !retryFutile ? (
        <Button variant={ButtonVariant.Secondary} onClick={onRetry}>
          Try again
        </Button>
      ) : null}
      {children}
    </div>
  );
}

/**
 * Whether a failed read can never succeed on a retry, so the page must not
 * keep showing its last loaded data: the resource is gone (`not_found`), the
 * caller lost access (`permission_denied`) or the session ended
 * (`unauthenticated`). Every other error (a network failure, `unavailable`,
 * `internal`, a deadline) is transient, so loaded data stays on screen.
 */
export function isTerminalError(error: unknown): boolean {
  return (
    error instanceof ConnectError &&
    (error.code === Code.NotFound ||
      error.code === Code.PermissionDenied ||
      error.code === Code.Unauthenticated)
  );
}

/** The part of a React Query result that the boundary reads. */
export interface BoundaryQuery<T> {
  data: T | undefined;
  error: unknown;
  isPending: boolean;
  isFetching: boolean;
  refetch: () => unknown;
}

/**
 * Whether a query failure replaces the content with a full error notice:
 * there is no loaded data to show, or the error is terminal. A failed
 * refetch of loaded data with a transient error returns false, because
 * React Query keeps `data` next to the `error` and the content, with its
 * open dialogs and typed values, must stay mounted.
 */
export function showsFullError(query: BoundaryQuery<unknown>): boolean {
  if (query.error === null || query.error === undefined) return false;
  return query.data === undefined || isTerminalError(query.error);
}

/**
 * The non-blocking notice of a failed refetch of loaded data. The content
 * stays visible and may be out of date. The notice goes away on the next
 * successful fetch, because the query then clears its error.
 */
export function RefreshFailedNotice({
  query,
}: {
  query: BoundaryQuery<unknown>;
}) {
  const billing = billingReasonOf(query.error) !== undefined;
  return (
    <div className="notice notice-error" role="alert">
      <p>
        The last refresh failed, so this page may be out of date.{" "}
        <TextWithAccountsLink text={errorMessage(query.error)} />
      </p>
      {billing ? null : (
        <Button
          variant={ButtonVariant.Secondary}
          disabled={query.isFetching}
          onClick={() => void query.refetch()}
        >
          Try again
        </Button>
      )}
    </div>
  );
}

/**
 * Renders the state of a query: a loading placeholder while the first load
 * runs, the full error notice when there is no data or the error is terminal
 * (see `showsFullError`), and otherwise the content. A failed refetch of
 * loaded data with a transient error renders the content with a
 * `RefreshFailedNotice` above it, so the content never unmounts.
 */
export function QueryBoundary<T>({
  query,
  loading,
  children,
}: {
  query: BoundaryQuery<T>;
  loading: string;
  children: (data: T) => ReactNode;
}) {
  const failed = query.error !== null && query.error !== undefined;
  if (query.isPending || (query.data === undefined && !failed)) {
    return <Loading label={loading} />;
  }
  if (showsFullError(query)) {
    return (
      <ErrorNotice error={query.error} onRetry={() => void query.refetch()} />
    );
  }
  return (
    <>
      {failed ? <RefreshFailedNotice query={query} /> : null}
      {children(query.data as T)}
    </>
  );
}

/**
 * The state of the pages that a list has not loaded yet: a status while the
 * console follows the continuation token, and the error with a retry when a
 * later page failed. It renders nothing when the list is complete.
 *
 * `noun` is the plural of the listed object, for example "API keys".
 */
export function MorePagesNotice({
  noun,
  pages,
}: {
  noun: string;
  pages: Pick<
    FollowedPages,
    "isIncomplete" | "loadMoreError" | "retryLoadMore"
  >;
}) {
  if (pages.loadMoreError) {
    return (
      <>
        <p className="muted">Some {noun} are not shown yet.</p>
        <ErrorNotice
          error={pages.loadMoreError}
          onRetry={pages.retryLoadMore}
        />
      </>
    );
  }
  if (pages.isIncomplete) {
    return (
      <p className="muted" role="status">
        Loading more {noun}
      </p>
    );
  }
  return null;
}

export function Empty({
  title,
  children,
}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      {children ? <p className="muted">{children}</p> : null}
    </div>
  );
}

/**
 * The horizontally scrolling wrapper of a table. The name and `tabIndex` make
 * the scroll region reachable and operable with the keyboard (arrow keys
 * scroll it once it has focus) and announce it to screen readers.
 */
export function TableRegion({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="table-wrap" role="region" aria-label={label} tabIndex={0}>
      {children}
    </div>
  );
}

const SANDBOX_BADGE: Partial<Record<SandboxState, BadgeVariant>> = {
  [SandboxState.RUNNING]: BadgeVariant.Accent,
  [SandboxState.FAILED]: BadgeVariant.Danger,
  [SandboxState.UNREACHABLE]: BadgeVariant.Danger,
};

export function SandboxStateBadge({
  state,
  lastKnownState,
}: {
  state: SandboxState;
  lastKnownState?: SandboxState | undefined;
}) {
  const text =
    state === SandboxState.UNREACHABLE && lastKnownState !== undefined
      ? `${labels.sandboxState[state]} (last ${labels.sandboxState[lastKnownState].toLowerCase()})`
      : labels.sandboxState[state];
  return (
    <Badge variant={SANDBOX_BADGE[state] ?? BadgeVariant.Neutral}>{text}</Badge>
  );
}

export function DeviceStateBadge({ state }: { state: DeviceState }) {
  const variant =
    state === DeviceState.ONLINE
      ? BadgeVariant.Accent
      : state === DeviceState.REMOVED
        ? BadgeVariant.Danger
        : BadgeVariant.Neutral;
  return <Badge variant={variant}>{labels.deviceState[state]}</Badge>;
}

/**
 * Shows a new secret once, with a copy action. The console keeps the
 * secret only in component state and drops it when the dialog closes.
 */
export function SecretDialog({
  title,
  label,
  secret,
  hint,
  onClose,
}: {
  title: string;
  label: string;
  secret: string | undefined;
  hint?: ReactNode;
  onClose: () => void;
}) {
  const copy = async () => {
    if (secret === undefined) return;
    try {
      await navigator.clipboard.writeText(secret);
      toast.success("Copied");
    } catch {
      toast.error("Copy failed. Select the text and copy it.");
    }
  };
  return (
    <Dialog
      open={secret !== undefined}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent>
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>
          It appears only once. Copy it now and keep it somewhere safe; OwnSand
          cannot show it again.
        </DialogDescription>
        <div className="secret">
          <span className="secret-label">{label}</span>
          <code className="secret-value" aria-label={label}>
            {secret}
          </code>
        </div>
        {hint ? <p className="muted">{hint}</p> : null}
        <DialogActions>
          <DialogClose>Done</DialogClose>
          <Button onClick={copy}>Copy</Button>
        </DialogActions>
      </DialogContent>
    </Dialog>
  );
}

/**
 * One opening of a dialog, as its request callback sees it. A dialog opening
 * ends on dismissal, a controlled close and unmount, and a request that
 * settles later can ask whether its opening is still the current one before
 * it changes anything that belongs to the opening: the route, the close of
 * the dialog, a secret display. Work that belongs to the data and not to the
 * opening, such as cache invalidation, runs whatever the answer is.
 */
export interface DialogOpening {
  /** Whether the opening that started the request is still the current one. */
  isCurrent(): boolean;
}

/**
 * The identity of the current opening of a dialog. Every open, dismissal,
 * controlled close and unmount ends the opening and starts a new one, so a
 * request that settles later can tell that its opening is over and must not
 * close or show an error on the next opening, which can be for another
 * target or for the same one.
 *
 * `begin` captures the opening when a request starts; `end` ends it on a
 * dismissal that the dialog itself sees.
 */
function useDialogOpening(open: boolean) {
  const opening = useRef(0);
  useEffect(
    () => () => {
      opening.current += 1;
    },
    [open],
  );
  return {
    begin(): DialogOpening {
      const mine = opening.current;
      return { isCurrent: () => opening.current === mine };
    },
    end() {
      opening.current += 1;
    },
  };
}

/**
 * An explicit confirmation step for a destructive action. The cancel
 * button comes first and gets the focus; Escape closes the dialog.
 *
 * `onConfirm` receives the opening that started the request. A caller that
 * redirects after the request succeeds does so only while the opening is
 * current, so a request that settles after Escape, Cancel or a click outside
 * does not move the user away from the page they went to since.
 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  onConfirm,
  onOpenChange,
}: {
  open: boolean;
  title: string;
  description: ReactNode;
  confirmLabel: string;
  onConfirm: (opening: DialogOpening) => Promise<unknown>;
  onOpenChange: (open: boolean) => void;
}) {
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    // Adjust state while rendering: every time the dialog opens again it
    // starts without the notice of an earlier failure, including a failure
    // that arrived after the dialog was already closed, and without the
    // pending state of an earlier request.
    setWasOpen(open);
    setPending(false);
    if (open) setError(undefined);
  }
  const dialogOpening = useDialogOpening(open);
  const confirm = async () => {
    if (pending) return;
    const mine = dialogOpening.begin();
    setPending(true);
    // A retry removes the notice of the previous failure.
    setError(undefined);
    try {
      await onConfirm(mine);
      if (mine.isCurrent()) onOpenChange(false);
    } catch (e) {
      if (mine.isCurrent()) setError(e);
    } finally {
      if (mine.isCurrent()) setPending(false);
    }
  };
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          dialogOpening.end();
          setPending(false);
          setError(undefined);
        }
        onOpenChange(next);
      }}
    >
      <AlertDialogContent>
        <AlertDialogTitle>{title}</AlertDialogTitle>
        <AlertDialogDescription>{description}</AlertDialogDescription>
        {error !== undefined ? <ErrorNotice error={error} /> : null}
        <AlertDialogActions>
          <AlertDialogClose>Cancel</AlertDialogClose>
          <Button
            variant={ButtonVariant.Danger}
            disabled={pending}
            onClick={confirm}
          >
            {confirmLabel}
          </Button>
        </AlertDialogActions>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/** A dialog with a form and one submit button. */
export function FormDialog({
  open,
  title,
  description,
  submitLabel,
  onSubmit,
  onOpenChange,
  children,
  wide = false,
  submitDisabled = false,
}: {
  open: boolean;
  title: string;
  description?: ReactNode;
  submitLabel: string;
  /** Disables the submit button, for example while there is nothing to submit. */
  submitDisabled?: boolean;
  /** A wider panel for forms with several fields per row. */
  wide?: boolean;
  /**
   * Sends the request of the form. `opening` is the opening that submitted
   * it. The caller changes what belongs to that opening (closing the form,
   * resetting its fields, showing a new secret or opening the new object's
   * page) only while `opening.isCurrent()`, so a request that settles after
   * Escape, Cancel or a click outside does not change the next opening.
   * Work that belongs to the data, such as cache invalidation, runs either
   * way.
   */
  onSubmit: (opening: DialogOpening) => Promise<unknown>;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
}) {
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    // Adjust state while rendering: every time the dialog opens again it
    // starts without the notice of an earlier failure, including a failure
    // that arrived after the dialog was already closed, and without the
    // pending state of an earlier request.
    setWasOpen(open);
    setPending(false);
    if (open) setError(undefined);
  }
  const dialogOpening = useDialogOpening(open);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (pending || submitDisabled) return;
    const mine = dialogOpening.begin();
    setPending(true);
    setError(undefined);
    try {
      await onSubmit(mine);
    } catch (e) {
      if (mine.isCurrent()) setError(e);
    } finally {
      // A request of an ended opening must not clear the pending state of
      // the request that the current opening sent.
      if (mine.isCurrent()) setPending(false);
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          dialogOpening.end();
          setPending(false);
          setError(undefined);
        }
        onOpenChange(next);
      }}
    >
      <DialogContent className={wide ? "dialog-wide" : undefined}>
        <DialogTitle>{title}</DialogTitle>
        {description ? (
          <DialogDescription>{description}</DialogDescription>
        ) : null}
        <form className="form" onSubmit={submit} noValidate>
          {children}
          {error !== undefined ? <ErrorNotice error={error} /> : null}
          <DialogActions>
            <DialogClose>Cancel</DialogClose>
            <Button type="submit" disabled={pending || submitDisabled}>
              {submitLabel}
            </Button>
          </DialogActions>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** A label and value pair of a details list. */
export function Detail({
  term,
  children,
}: {
  term: string;
  children: ReactNode;
}) {
  return (
    <div className="detail">
      <dt>{term}</dt>
      <dd>{children}</dd>
    </div>
  );
}

/**
 * Shortens a UUID to its first 8 and last 4 characters. A UUID v7 starts with
 * a timestamp, so the first 8 characters alone repeat for about 65 seconds;
 * the last 4 characters tell ids from the same period apart.
 */
export function shortIdText(id: string): string {
  return id.length > 13 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id;
}

/** Shortens a UUID for display; the full value stays in the title. */
export function ShortId({ id }: { id: string }) {
  return (
    <code className="id" title={id}>
      {shortIdText(id)}
    </code>
  );
}
