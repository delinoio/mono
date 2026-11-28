// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
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
import {
  type FormEvent,
  type ReactNode,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { useAuth } from "../auth.tsx";
import { errorMessage } from "../format.ts";
import { type Translate, useI18n } from "../i18n.ts";
import { useRouter } from "../router.tsx";

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

/** Shows a failed action in a toast. */
export function toastError(error: unknown, t: Translate): void {
  toast.error(errorMessage(error, t));
}

/** Reports direct action failures while keeping successful page data visible. */
export function useActionSignInNotice() {
  const { isCurrentVisit } = useRouter();
  const { t } = useI18n();
  const [failure, setFailure] = useState<{
    visit: typeof isCurrentVisit;
    error: ConnectError;
  }>();

  function reportActionError(error: unknown) {
    if (error instanceof ConnectError && error.code === Code.Unauthenticated) {
      if (isCurrentVisit()) setFailure({ visit: isCurrentVisit, error });
      return;
    }
    toastError(error, t);
  }

  return {
    // A batched leave and return can reuse this component for a new visit.
    signInError: failure?.visit === isCurrentVisit ? failure.error : undefined,
    reportActionError,
  };
}

/** A loading placeholder for a list. */
export function Loading({ label }: { label?: string }) {
  const { t } = useI18n();
  return (
    <div
      className="loading"
      role="status"
      aria-label={label ?? t("common.loading")}
    >
      <Skeleton height="56px" />
      <Skeleton height="56px" />
      <Skeleton height="56px" />
    </div>
  );
}

export function SignInAction({ variant }: { variant?: ButtonVariant }) {
  const auth = useAuth();
  const { t } = useI18n();
  return (
    <>
      <Button type="button" variant={variant} onClick={() => auth.signIn()}>
        {t("auth.signIn")}
      </Button>
      {auth.signInFailed ? (
        <p className="sign-in-failure" role="alert">
          {t("auth.signInFailed")}
        </p>
      ) : null}
    </>
  );
}

/** A failed request with the recovery action for its error. */
export function ErrorNotice({
  title,
  error,
  onRetry,
}: {
  title?: string;
  error: unknown;
  onRetry?: () => void;
}) {
  const { t } = useI18n();
  if (error instanceof ConnectError && error.code === Code.Unauthenticated) {
    return (
      <div className="notice notice-error">
        <p role="alert">{errorMessage(error, t)}</p>
        <SignInAction variant={ButtonVariant.Secondary} />
      </div>
    );
  }
  return (
    <div className="notice notice-error" role="alert">
      <p>
        {title ? `${title} ` : null}
        {errorMessage(error, t, title === undefined ? undefined : "")}
      </p>
      {onRetry ? (
        <Button
          type="button"
          variant={ButtonVariant.Secondary}
          onClick={onRetry}
        >
          {t("common.tryAgain")}
        </Button>
      ) : null}
    </div>
  );
}

function isTerminalError(error: unknown): boolean {
  return (
    error instanceof ConnectError &&
    (error.code === Code.NotFound ||
      error.code === Code.PermissionDenied ||
      error.code === Code.Unauthenticated)
  );
}

interface BoundaryQuery<T> {
  data: T | undefined;
  error: unknown;
  isPending: boolean;
  isFetching: boolean;
  isFetchNextPageError?: boolean;
  refetch: () => unknown;
  retryNextPage?: () => void;
}

export function showsFullError(query: BoundaryQuery<unknown>): boolean {
  return (
    query.error != null &&
    (query.data === undefined || isTerminalError(query.error))
  );
}

export function RefreshFailedNotice({
  query,
}: {
  query: BoundaryQuery<unknown>;
}) {
  const { t } = useI18n();
  return (
    <div className="notice notice-error" role="alert">
      <p>
        {t("errors.refreshFailed", {
          message: errorMessage(query.error, t),
        })}
      </p>
      <Button
        type="button"
        variant={ButtonVariant.Secondary}
        disabled={query.isFetching}
        onClick={() => void query.refetch()}
      >
        {t("common.tryAgain")}
      </Button>
    </div>
  );
}

/** Keeps loaded content mounted during a transient refresh failure. */
export function QueryBoundary<T>({
  query,
  loading,
  errorTitle,
  children,
}: {
  query: BoundaryQuery<T>;
  loading: string;
  errorTitle?: string;
  children: (data: T) => ReactNode;
}) {
  if (showsFullError(query)) {
    return (
      <ErrorNotice
        title={errorTitle}
        error={query.error}
        onRetry={() => void query.refetch()}
      />
    );
  }
  if (query.isPending || query.data === undefined) {
    return <Loading label={loading} />;
  }
  return (
    <>
      {query.error != null && !query.isFetchNextPageError ? (
        <RefreshFailedNotice query={query} />
      ) : null}
      {query.isFetchNextPageError &&
      !query.isFetching &&
      query.retryNextPage ? (
        <ErrorNotice error={query.error} onRetry={query.retryNextPage} />
      ) : null}
      {children(query.data)}
    </>
  );
}

/** Retries the continuation separately from a refresh of the loaded pages. */
export function ListPagination({
  query,
}: {
  query: BoundaryQuery<unknown> & {
    hasNextPage: boolean;
    isFetchNextPageError: boolean;
    fetchNextPage: (options: { cancelRefetch: boolean }) => unknown;
  };
}) {
  const { t } = useI18n();
  if (!query.hasNextPage || showsFullError(query)) return null;

  const loadMore = () => void query.fetchNextPage({ cancelRefetch: false });
  if (query.isFetchNextPageError && !query.isFetching) {
    return <ErrorNotice error={query.error} onRetry={loadMore} />;
  }
  return (
    <Button
      variant={ButtonVariant.Secondary}
      disabled={query.isFetching}
      onClick={loadMore}
    >
      {t("common.showMore")}
    </Button>
  );
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

/** A state label; failures use the danger color, active states the accent. */
export function StateBadge({
  text,
  tone = BadgeVariant.Neutral,
}: {
  text: string;
  tone?: BadgeVariant;
}) {
  return <Badge variant={tone}>{text}</Badge>;
}

export type DialogOpening = { isCurrent(): boolean };

function useDialogOpening(open: boolean) {
  const opening = useRef(0);
  const end = () => {
    opening.current += 1;
  };
  useLayoutEffect(() => {
    // Invalidate the opening during a controlled close or unmount.
    return () => {
      opening.current += 1;
    };
  }, [open]);

  return {
    begin(): DialogOpening {
      const startedIn = opening.current;
      return { isCurrent: () => opening.current === startedIn };
    },
    end,
  };
}

/**
 * An explicit confirmation step for a destructive action. The cancel
 * button comes first and gets the focus; Escape closes the dialog.
 * Check the opening before redirecting after a successful request. Cache
 * invalidation belongs to the request and runs even after dismissal.
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
  const { t } = useI18n();
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    setPending(false);
    setError(undefined);
  }

  const dialogOpening = useDialogOpening(open);
  const confirm = async () => {
    if (pending) return;
    const opening = dialogOpening.begin();
    setPending(true);
    setError(undefined);
    try {
      await onConfirm(opening);
      if (opening.isCurrent()) onOpenChange(false);
    } catch (e) {
      if (opening.isCurrent()) setError(e);
    } finally {
      if (opening.isCurrent()) setPending(false);
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
          <AlertDialogClose>{t("common.cancel")}</AlertDialogClose>
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
  /** Apply dialog follow-up only while the opening is current. Refresh data either way. */
  onSubmit: (opening: DialogOpening) => Promise<unknown>;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
}) {
  const { t } = useI18n();
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    setPending(false);
    setError(undefined);
  }
  const dialogOpening = useDialogOpening(open);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!open || pending || submitDisabled) return;
    const opening = dialogOpening.begin();
    setPending(true);
    setError(undefined);
    try {
      await onSubmit(opening);
    } catch (e) {
      if (opening.isCurrent()) setError(e);
    } finally {
      if (opening.isCurrent()) setPending(false);
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
            <DialogClose>{t("common.cancel")}</DialogClose>
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

/** Shortens a UUID for display; the full value stays in the title. */
export function ShortId({ id }: { id: string }) {
  return (
    <code className="id" title={id}>
      {id.length > 13 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id}
    </code>
  );
}
