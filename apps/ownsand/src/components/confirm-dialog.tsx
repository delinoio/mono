// SPDX-License-Identifier: Apache-2.0
import { ButtonVariant } from "delino-ui/button";
import {
  AlertDialog,
  AlertDialogActions,
  AlertDialogClose,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogTitle,
} from "delino-ui/alert-dialog";
import { useState } from "react";
import { errorMessage } from "../api/errors.ts";

export interface ConfirmDialogProps {
  readonly open: boolean;
  readonly title: string;
  readonly description: string;
  /** Label of the destructive answer, for example "Revoke API key". */
  readonly confirmLabel: string;
  /** Runs the action; the dialog stays open until it settles. */
  readonly onConfirm: () => Promise<void>;
  readonly onClose: () => void;
}

/**
 * The explicit confirmation step for destructive actions. Focus starts on
 * "Cancel", Escape closes it, and the action runs only from the confirm
 * button. A failure keeps the dialog open with a message.
 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  onConfirm,
  onClose,
}: ConfirmDialogProps) {
  const [error, setError] = useState<string | undefined>(undefined);
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          setError(undefined);
          onClose();
        }
      }}
    >
      <AlertDialogContent>
        <AlertDialogTitle>{title}</AlertDialogTitle>
        <AlertDialogDescription>{description}</AlertDialogDescription>
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : null}
        <AlertDialogActions>
          <AlertDialogClose value="cancel">Cancel</AlertDialogClose>
          <AlertDialogClose
            value="confirm"
            variant={ButtonVariant.Danger}
            action={async () => {
              setError(undefined);
              try {
                await onConfirm();
              } catch (caught) {
                setError(errorMessage(caught));
                // Keep the dialog open so the user sees the failure.
                throw caught;
              }
            }}
          >
            {confirmLabel}
          </AlertDialogClose>
        </AlertDialogActions>
      </AlertDialogContent>
    </AlertDialog>
  );
}
