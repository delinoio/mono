// SPDX-License-Identifier: Apache-2.0
import {
  Dialog as DialogRoot,
  DialogBackdrop,
  DialogClose as DialogClosePart,
  DialogContent as DialogContentPart,
  DialogDescription as DialogDescriptionPart,
  DialogTitle as DialogTitlePart,
  DialogTrigger as DialogTriggerPart,
} from "@ui-sdk/react/dialog";
import type * as React from "react";
import { ButtonSize, ButtonVariant } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export { createDialogController } from "@ui-sdk/react/dialog";
export type { DialogController } from "@ui-sdk/react/dialog";

/**
 * The props of `Dialog`. `dismissible` is left out because every dialog in
 * Delino apps must close with the Escape key. `modal` and `presentation` are
 * left out because every dialog is a centered modal with a backdrop; use
 * `Drawer` for an edge sheet.
 */
export type DialogProps = Omit<
  React.ComponentProps<typeof DialogRoot>,
  "dismissible" | "modal" | "presentation"
>;

/**
 * A modal dialog built from UI SDK `Dialog`. It is always centered and modal:
 * it traps focus, closes with Escape or a press outside, and returns focus to
 * its trigger. Use `Drawer` for an edge sheet.
 */
export function Dialog(props: DialogProps) {
  // The type already omits these props, but types do not stop JS callers or
  // untyped spread objects. Drop the keys at runtime so Escape always closes
  // and the dialog stays modal and centered.
  const {
    dismissible: _dismissible,
    modal: _modal,
    presentation: _presentation,
    ...rest
  } = props as DialogProps & {
    dismissible?: unknown;
    modal?: unknown;
    presentation?: unknown;
  };
  return <DialogRoot {...rest} />;
}

type ButtonStyleProps = {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

export type DialogTriggerProps = React.ComponentProps<
  typeof DialogTriggerPart
> &
  ButtonStyleProps;

/** The button that opens the dialog. It looks like a `Button`. */
export function DialogTrigger({
  variant = ButtonVariant.Primary,
  size = ButtonSize.Medium,
  className,
  ...props
}: DialogTriggerProps) {
  return (
    <DialogTriggerPart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}

export type DialogContentProps = React.ComponentProps<typeof DialogContentPart>;

/** The dialog panel, centered over a dimmed page. */
export function DialogContent({ className, ...props }: DialogContentProps) {
  return (
    <>
      <DialogBackdrop className="dui-dialog-backdrop" />
      <DialogContentPart
        className={mergeClassName("dui-dialog-content", className)}
        {...props}
      />
    </>
  );
}

export type DialogTitleProps = React.ComponentProps<typeof DialogTitlePart>;

/** The title that names the dialog. A level 2 heading. */
export function DialogTitle({ className, ...props }: DialogTitleProps) {
  return (
    <DialogTitlePart
      className={mergeClassName("dui-dialog-title", className)}
      {...props}
    />
  );
}

export type DialogDescriptionProps = React.ComponentProps<
  typeof DialogDescriptionPart
>;

/** Text that describes the dialog. */
export function DialogDescription({
  className,
  ...props
}: DialogDescriptionProps) {
  return (
    <DialogDescriptionPart
      className={mergeClassName("dui-dialog-description", className)}
      {...props}
    />
  );
}

export type DialogActionsProps = React.ComponentProps<"div">;

/** The row of buttons at the bottom of the dialog. */
export function DialogActions({ className, ...props }: DialogActionsProps) {
  return (
    <div
      className={
        className ? `dui-dialog-actions ${className}` : "dui-dialog-actions"
      }
      {...props}
    />
  );
}

export type DialogCloseProps = React.ComponentProps<typeof DialogClosePart> &
  ButtonStyleProps;

/**
 * A button that closes the dialog. A Promise returned from `action` keeps
 * the dialog open until it settles.
 */
export function DialogClose({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Medium,
  className,
  ...props
}: DialogCloseProps) {
  return (
    <DialogClosePart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}
