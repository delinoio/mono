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
import { DeviceState, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { type FormEvent, type ReactNode, useState } from "react";
import { billingReasonOf } from "../api.ts";
import { ACCOUNTS_URL } from "../config.ts";
import { errorMessage, labels } from "../format.ts";

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
 * A failed request, with a retry button. A billing error shows the server
 * message with its link and never offers a retry, because it is terminal.
 */
export function ErrorNotice({
  error,
  onRetry,
}: {
  error: unknown;
  onRetry?: () => void;
}) {
  const billing = billingReasonOf(error) !== undefined;
  return (
    <div className="notice notice-error" role="alert">
      <p>
        <TextWithAccountsLink text={errorMessage(error)} />
      </p>
      {onRetry && !billing ? (
        <Button variant={ButtonVariant.Secondary} onClick={onRetry}>
          Try again
        </Button>
      ) : null}
    </div>
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
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    // Adjust state while rendering: every time the dialog opens again it
    // starts without the notice of an earlier failure, including a failure
    // that arrived after the dialog was already closed.
    setWasOpen(open);
    if (open) setError(undefined);
  }
  const confirm = async () => {
    if (pending) return;
    setPending(true);
    // A retry removes the notice of the previous failure.
    setError(undefined);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch (e) {
      setError(e);
    } finally {
      setPending(false);
    }
  };
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!next) setError(undefined);
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
  onSubmit: () => Promise<unknown>;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
}) {
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    // Adjust state while rendering: every time the dialog opens again it
    // starts without the notice of an earlier failure, including a failure
    // that arrived after the dialog was already closed.
    setWasOpen(open);
    if (open) setError(undefined);
  }
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (pending || submitDisabled) return;
    setPending(true);
    setError(undefined);
    try {
      await onSubmit();
    } catch (e) {
      setError(e);
    } finally {
      setPending(false);
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) setError(undefined);
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

/** Shortens a UUID for display; the full value stays in the title. */
export function ShortId({ id }: { id: string }) {
  return (
    <code className="id" title={id}>
      {id.length > 13 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id}
    </code>
  );
}
