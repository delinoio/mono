// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
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
import {
  type FormEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import { useAuth } from "../auth.tsx";
import { errorMessage } from "../format.ts";
import type { AllPages } from "../paging.ts";

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
 * A failed request. A request that ended with `Code.Unauthenticated` offers
 * the sign-in redirect, because retrying cannot succeed: the Logto SDK keeps
 * `isAuthenticated` true after a failed token refresh, so neither the route
 * guard nor the top bar would offer it. Any other failure offers a retry
 * when `onRetry` is set.
 *
 * `onSignIn` replaces the default `useAuth().signIn()` for a caller that has
 * to prepare the redirect first.
 */
export function ErrorNotice({
  error,
  onRetry,
  onSignIn,
}: {
  error: unknown;
  onRetry?: () => void;
  onSignIn?: () => Promise<void> | void;
}) {
  const unauthenticated =
    error instanceof ConnectError && error.code === Code.Unauthenticated;
  return (
    <div className="notice notice-error" role="alert">
      <p>{errorMessage(error)}</p>
      {unauthenticated ? (
        <SignInAgainButton onSignIn={onSignIn} />
      ) : onRetry ? (
        <Button variant={ButtonVariant.Secondary} onClick={onRetry}>
          Try again
        </Button>
      ) : null}
    </div>
  );
}

/**
 * A failed background refetch of data that is already on screen (a window
 * focus refetch, a poll, or the refetch after a mutation). Render it above
 * the loaded content, and keep that content: the content, its selected tab,
 * its open dialogs and its typed input must survive a transient failure.
 * Only a request that failed before it ever returned data replaces the
 * content with an `ErrorNotice`.
 *
 * Pass the error of the failed refetch, or nothing while the last request
 * succeeded. The retry clears the notice when it succeeds. Keep the notice in
 * the same position of the tree in every state, so that the content below
 * does not remount when the notice appears.
 */
export function RefetchNotice({
  error,
  onRetry,
}: {
  error: unknown;
  onRetry: () => void;
}) {
  return error === undefined || error === null ? null : (
    <ErrorNotice error={error} onRetry={onRetry} />
  );
}

/**
 * The part of a TanStack infinite query result that `InfiniteListNotice`
 * reads. Any `useInfiniteQuery` result satisfies it.
 */
export interface InfiniteListState {
  error: unknown;
  isRefetchError: boolean;
  isFetchNextPageError: boolean;
  refetch: () => unknown;
  fetchNextPage: (options?: { cancelRefetch?: boolean }) => unknown;
}

/**
 * The notice of a list that the user reads on with a button. It keeps the
 * loaded rows and shows one of two failures above them:
 *
 * - A failed refresh of the loaded pages (`isRefetchError`) retries with
 *   `refetch`, which reads the list again from the first page.
 * - A failed request for the next page (`isFetchNextPageError`) retries with
 *   `fetchNextPage`, which requests the failed cursor again and appends the
 *   rows once. TanStack Query excludes this failure from `isRefetchError`,
 *   so the notice has to read it separately.
 *
 * A failed first request is not handled here: it replaces the section with
 * an `ErrorNotice`. An `Unauthenticated` failure offers the sign-in action
 * in both cases, like `ErrorNotice`. Render it in the same tree position in
 * every state, like `RefetchNotice`.
 */
export function InfiniteListNotice({ list }: { list: InfiniteListState }) {
  if (list.isFetchNextPageError) {
    return (
      <RefetchNotice
        error={list.error}
        onRetry={() => void list.fetchNextPage({ cancelRefetch: false })}
      />
    );
  }
  return (
    <RefetchNotice
      error={list.isRefetchError ? list.error : undefined}
      onRetry={() => void list.refetch()}
    />
  );
}

/**
 * The state of the pages after the first one: a short loading line while
 * later pages load and an error with a retry that resumes at the failed page,
 * or the sign-in action when the session ended. The items of the earlier pages
 * stay visible above it.
 */
export function MorePages({ pages }: { pages: AllPages<unknown> }) {
  if (pages.continuationError !== undefined) {
    // An ended session cannot recover through a retry, so offer the sign-in
    // action instead, like `ErrorNotice`. The loaded rows stay either way.
    const unauthenticated =
      pages.continuationError instanceof ConnectError &&
      pages.continuationError.code === Code.Unauthenticated;
    return (
      <div className="notice notice-error" role="alert">
        <p>Some items did not load. {errorMessage(pages.continuationError)}</p>
        {unauthenticated ? (
          <SignInAgainButton onSignIn={undefined} />
        ) : (
          <Button
            variant={ButtonVariant.Secondary}
            onClick={pages.retryContinuation}
          >
            Try again
          </Button>
        )}
      </div>
    );
  }
  return pages.isLoadingMore ? (
    <p className="muted" role="status">
      Loading more
    </p>
  ) : null;
}

/**
 * Starts the sign-in redirect. It reads the auth context only when it is
 * rendered, so an `ErrorNotice` for another failure needs no auth provider.
 */
function SignInAgainButton({
  onSignIn,
}: {
  onSignIn: (() => Promise<void> | void) | undefined;
}) {
  const auth = useAuth();
  return (
    <>
      <Button
        variant={ButtonVariant.Secondary}
        onClick={() => void (onSignIn ? onSignIn() : auth.signIn())}
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

enum SubmissionResult {
  /** Another submission of this opening is still running. */
  Skipped = "skipped",
  Succeeded = "succeeded",
  Failed = "failed",
  /** The dialog closed or opened again before the action settled. */
  Stale = "stale",
}

/**
 * The submission state of a dialog. One submission runs at a time, and its
 * outcome belongs to the opening of the dialog that started it: a request
 * that settles after the dialog closed or opened again changes no state and
 * reports `Stale`, so an earlier failure never shows on a later opening and
 * never keeps its submit button disabled.
 */
function useDialogSubmission(open: boolean) {
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    // Adjust state while rendering: every time the dialog opens again it
    // starts without the notice and the pending state of an earlier
    // submission, including a failure that arrived after the dialog closed.
    setWasOpen(open);
    if (open) {
      setError(undefined);
      setPending(false);
    }
  }
  // The opening counter changes on every open and close edge. The busy ref
  // stops a second submission in the same tick, before the pending state
  // renders, and starts again with every opening.
  const opening = useRef(0);
  const busy = useRef(false);
  useEffect(() => {
    opening.current += 1;
    busy.current = false;
  }, [open]);
  const clearError = useCallback(() => setError(undefined), []);
  const submit = useCallback(
    async (action: () => Promise<unknown>): Promise<SubmissionResult> => {
      if (busy.current) return SubmissionResult.Skipped;
      busy.current = true;
      const mine = opening.current;
      setPending(true);
      // A retry removes the notice of the previous failure.
      setError(undefined);
      try {
        await action();
        return mine === opening.current
          ? SubmissionResult.Succeeded
          : SubmissionResult.Stale;
      } catch (e) {
        if (mine !== opening.current) return SubmissionResult.Stale;
        setError(e);
        return SubmissionResult.Failed;
      } finally {
        if (mine === opening.current) {
          busy.current = false;
          setPending(false);
        }
      }
    },
    [],
  );
  return { error, pending, clearError, submit };
}

/**
 * An explicit confirmation step for a destructive action. The cancel
 * button comes first and gets the focus; Escape closes the dialog.
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
  onConfirm: () => Promise<unknown>;
  onOpenChange: (open: boolean) => void;
}) {
  const { error, pending, clearError, submit } = useDialogSubmission(open);
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!next) clearError();
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
            onClick={async () => {
              const result = await submit(onConfirm);
              if (result === SubmissionResult.Succeeded) onOpenChange(false);
            }}
          >
            {confirmLabel}
          </Button>
        </AlertDialogActions>
      </AlertDialogContent>
    </AlertDialog>
  );
}

export enum FormSubmitOutcome {
  /** The input passed the checks and the request was sent and succeeded. */
  Submitted = "submitted",
  /** The input failed a check; the form shows the message and sent nothing. */
  Invalid = "invalid",
}

/**
 * What `FormDialog.onSubmit` resolves with. `onSucceeded` holds the effects
 * that belong to the opening that submitted the form (close, reset, navigate,
 * toast). The dialog runs it only when that opening is still the current one,
 * so a request that succeeds after the dialog closed or opened again cannot
 * close the new opening, clear its draft, navigate or notify. Work that must
 * happen for every success, such as refreshing cached data, stays in
 * `onSubmit` before it resolves.
 */
export type FormSubmission =
  | { outcome: FormSubmitOutcome.Invalid }
  | { outcome: FormSubmitOutcome.Submitted; onSucceeded?: () => void };

/** The submission of a form whose input failed a check. */
export function formInvalid(): FormSubmission {
  return { outcome: FormSubmitOutcome.Invalid };
}

/** The submission of a form whose request succeeded. */
export function formSubmitted(onSucceeded?: () => void): FormSubmission {
  return { outcome: FormSubmitOutcome.Submitted, onSucceeded };
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
}: {
  open: boolean;
  title: string;
  description?: ReactNode;
  submitLabel: string;
  /** A wider panel for forms with several fields per row. */
  wide?: boolean;
  onSubmit: () => Promise<FormSubmission>;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
}) {
  const { error, pending, clearError, submit: run } = useDialogSubmission(open);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    // The holder keeps the submission outside the action, because the action
    // can settle after the opening that started it is gone.
    const settled: { submission?: FormSubmission } = {};
    const result = await run(async () => {
      settled.submission = await onSubmit();
    });
    if (
      result === SubmissionResult.Succeeded &&
      settled.submission?.outcome === FormSubmitOutcome.Submitted
    ) {
      settled.submission.onSucceeded?.();
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) clearError();
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
            <Button type="submit" disabled={pending}>
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
