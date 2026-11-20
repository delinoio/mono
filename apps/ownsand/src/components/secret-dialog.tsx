// SPDX-License-Identifier: Apache-2.0
import { Button, ButtonVariant } from "delino-ui/button";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "delino-ui/dialog";
import { useEffect, useState } from "react";

/** Copy state of the secret dialog. */
export enum CopyState {
  Idle = "idle",
  Copied = "copied",
  Failed = "failed",
}

export interface SecretDialogProps {
  /** The secret to show once, or `undefined` when the dialog is closed. */
  readonly secret: string | undefined;
  readonly title: string;
  /** What the secret is for, shown above it. */
  readonly description: string;
  /** Accessible name of the secret value, for example "Invitation link". */
  readonly secretLabel: string;
  /** Called when the dialog closes; the caller must drop the secret. */
  readonly onClose: () => void;
  /** Clipboard writer, replaceable in tests. */
  readonly writeClipboard?: (text: string) => Promise<void>;
}

function defaultWriteClipboard(text: string): Promise<void> {
  return navigator.clipboard.writeText(text);
}

/**
 * Shows a new secret once, with a copy action and a statement that it will
 * not appear again. The secret lives only in the caller's React state and is
 * dropped when the dialog closes; it is never written to storage or logs.
 */
export function SecretDialog({
  secret,
  title,
  description,
  secretLabel,
  onClose,
  writeClipboard = defaultWriteClipboard,
}: SecretDialogProps) {
  const [copyState, setCopyState] = useState(CopyState.Idle);
  useEffect(() => {
    setCopyState(CopyState.Idle);
  }, [secret]);

  const copy = async () => {
    if (secret === undefined) {
      return;
    }
    try {
      await writeClipboard(secret);
      setCopyState(CopyState.Copied);
    } catch {
      setCopyState(CopyState.Failed);
    }
  };

  return (
    <Dialog
      open={secret !== undefined}
      onOpenChange={(open) => {
        if (!open) {
          onClose();
        }
      }}
    >
      <DialogContent>
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>{description}</DialogDescription>
        <p className="secret-warning" role="note">
          This will not be shown again. Copy it now and keep it somewhere safe.
        </p>
        <label className="secret-label" htmlFor="one-time-secret">
          {secretLabel}
        </label>
        <textarea
          id="one-time-secret"
          className="secret-value"
          readOnly
          rows={3}
          value={secret ?? ""}
          onFocus={(event) => event.currentTarget.select()}
        />
        <p className="secret-status" role="status" aria-live="polite">
          {copyState === CopyState.Copied
            ? "Copied to the clipboard."
            : copyState === CopyState.Failed
              ? "Copying failed. Select the text and copy it manually."
              : ""}
        </p>
        <DialogActions>
          <Button variant={ButtonVariant.Primary} onClick={copy} autoFocus>
            Copy
          </Button>
          <DialogClose>Done</DialogClose>
        </DialogActions>
      </DialogContent>
    </Dialog>
  );
}
