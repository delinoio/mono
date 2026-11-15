// SPDX-License-Identifier: Apache-2.0
import {
  AlertDialog as AlertDialogRoot,
  AlertDialogBackdrop,
  AlertDialogClose as AlertDialogClosePart,
  AlertDialogContent as AlertDialogContentPart,
  AlertDialogDescription as AlertDialogDescriptionPart,
  AlertDialogTitle as AlertDialogTitlePart,
  AlertDialogTrigger as AlertDialogTriggerPart,
} from "@ui-sdk/react/alert-dialog";
import type * as React from "react";
import { ButtonSize, ButtonVariant } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export {
  createAlertDialogController,
  createConfirm,
} from "@ui-sdk/react/alert-dialog";
export type { AlertDialogController } from "@ui-sdk/react/alert-dialog";

/**
 * The props of `AlertDialog`. `dismissible` is left out because every dialog
 * in Delino apps must close with the Escape key.
 */
export type AlertDialogProps = Omit<
  React.ComponentProps<typeof AlertDialogRoot>,
  "dismissible"
>;

/**
 * A modal dialog that asks a question and waits for an answer, built from
 * UI SDK `AlertDialog`. It opens with focus on the first `AlertDialogClose`,
 * so put the least destructive answer first. Escape closes it; a press
 * outside does not.
 */
export function AlertDialog(props: AlertDialogProps) {
  // The type already omits `dismissible`, but types do not stop JS callers or
  // untyped spread objects. Drop the key at runtime so Escape always closes.
  const { dismissible: _dismissible, ...rest } = props as AlertDialogProps & {
    dismissible?: unknown;
  };
  return <AlertDialogRoot {...rest} />;
}

type ButtonStyleProps = {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

export type AlertDialogTriggerProps = React.ComponentProps<
  typeof AlertDialogTriggerPart
> &
  ButtonStyleProps;

/** The button that opens the dialog. It looks like a `Button`. */
export function AlertDialogTrigger({
  variant = ButtonVariant.Primary,
  size = ButtonSize.Medium,
  className,
  ...props
}: AlertDialogTriggerProps) {
  return (
    <AlertDialogTriggerPart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}

export type AlertDialogContentProps = React.ComponentProps<
  typeof AlertDialogContentPart
>;

/** The dialog panel, centered over a dimmed page. */
export function AlertDialogContent({
  className,
  ...props
}: AlertDialogContentProps) {
  return (
    <>
      <AlertDialogBackdrop className="dui-dialog-backdrop" />
      <AlertDialogContentPart
        className={mergeClassName("dui-dialog-content", className)}
        {...props}
      />
    </>
  );
}

export type AlertDialogTitleProps = React.ComponentProps<
  typeof AlertDialogTitlePart
>;

/** The question. A level 2 heading that names the dialog. */
export function AlertDialogTitle({
  className,
  ...props
}: AlertDialogTitleProps) {
  return (
    <AlertDialogTitlePart
      className={mergeClassName("dui-dialog-title", className)}
      {...props}
    />
  );
}

export type AlertDialogDescriptionProps = React.ComponentProps<
  typeof AlertDialogDescriptionPart
>;

/** What happens if the user agrees. */
export function AlertDialogDescription({
  className,
  ...props
}: AlertDialogDescriptionProps) {
  return (
    <AlertDialogDescriptionPart
      className={mergeClassName("dui-dialog-description", className)}
      {...props}
    />
  );
}

export type AlertDialogActionsProps = React.ComponentProps<"div">;

/** The row of answer buttons at the bottom of the dialog. */
export function AlertDialogActions({
  className,
  ...props
}: AlertDialogActionsProps) {
  return (
    <div
      className={
        className ? `dui-dialog-actions ${className}` : "dui-dialog-actions"
      }
      {...props}
    />
  );
}

export type AlertDialogCloseProps = React.ComponentProps<
  typeof AlertDialogClosePart
> &
  ButtonStyleProps;

/**
 * An answer button that closes the dialog and reports its `value`. Use
 * `value="confirm"` for the agreeing answer. A Promise returned from
 * `action` keeps the dialog open until it settles.
 */
export function AlertDialogClose({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Medium,
  className,
  ...props
}: AlertDialogCloseProps) {
  return (
    <AlertDialogClosePart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}
