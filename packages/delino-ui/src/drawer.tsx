// SPDX-License-Identifier: Apache-2.0
import {
  Drawer as DrawerRoot,
  DrawerBackdrop,
  DrawerClose as DrawerClosePart,
  DrawerContent as DrawerContentPart,
  DrawerDescription as DrawerDescriptionPart,
  DrawerTitle as DrawerTitlePart,
  DrawerTrigger as DrawerTriggerPart,
} from "@ui-sdk/react/drawer";
import type * as React from "react";
import { ButtonSize, ButtonVariant } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export { createDrawerController } from "@ui-sdk/react/drawer";
export type { DrawerController, DrawerSide } from "@ui-sdk/react/drawer";

/**
 * The props of `Drawer`. `dismissible` is left out because every dialog in
 * Delino apps must close with the Escape key.
 */
export type DrawerProps = Omit<
  React.ComponentProps<typeof DrawerRoot>,
  "dismissible"
>;

/**
 * A sheet that slides in from an edge of the screen, built from UI SDK
 * `Drawer`. It opens from the bottom by default (`side`). It traps focus,
 * closes with Escape, a press outside or a swipe toward its edge, and
 * returns focus to its trigger.
 */
export function Drawer(props: DrawerProps) {
  // The type already omits `dismissible`, but types do not stop JS callers or
  // untyped spread objects. Drop the key at runtime so Escape always closes.
  const { dismissible: _dismissible, ...rest } = props as DrawerProps & {
    dismissible?: unknown;
  };
  return <DrawerRoot {...rest} />;
}

type ButtonStyleProps = {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

export type DrawerTriggerProps = React.ComponentProps<
  typeof DrawerTriggerPart
> &
  ButtonStyleProps;

/** The button that opens the drawer. It looks like a `Button`. */
export function DrawerTrigger({
  variant = ButtonVariant.Primary,
  size = ButtonSize.Medium,
  className,
  ...props
}: DrawerTriggerProps) {
  return (
    <DrawerTriggerPart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}

export type DrawerContentProps = React.ComponentProps<typeof DrawerContentPart>;

/**
 * The sheet. It also renders the backdrop. The drawer moves the sheet and
 * fades the backdrop itself, so do not style their `translate` or
 * `opacity`.
 */
export function DrawerContent({ className, ...props }: DrawerContentProps) {
  return (
    <>
      <DrawerBackdrop className="dui-drawer-backdrop" />
      <DrawerContentPart
        className={mergeClassName("dui-drawer-content", className)}
        {...props}
      />
    </>
  );
}

export type DrawerTitleProps = React.ComponentProps<typeof DrawerTitlePart>;

/** The title that names the drawer. A level 2 heading. */
export function DrawerTitle({ className, ...props }: DrawerTitleProps) {
  return (
    <DrawerTitlePart
      className={mergeClassName("dui-dialog-title", className)}
      {...props}
    />
  );
}

export type DrawerDescriptionProps = React.ComponentProps<
  typeof DrawerDescriptionPart
>;

/** Text that describes the drawer. */
export function DrawerDescription({
  className,
  ...props
}: DrawerDescriptionProps) {
  return (
    <DrawerDescriptionPart
      className={mergeClassName("dui-dialog-description", className)}
      {...props}
    />
  );
}

export type DrawerActionsProps = React.ComponentProps<"div">;

/** The row of buttons at the bottom of the drawer. */
export function DrawerActions({ className, ...props }: DrawerActionsProps) {
  return (
    <div
      className={
        className ? `dui-drawer-actions ${className}` : "dui-drawer-actions"
      }
      {...props}
    />
  );
}

export type DrawerCloseProps = React.ComponentProps<typeof DrawerClosePart> &
  ButtonStyleProps;

/**
 * A button that closes the drawer. A Promise returned from `action` keeps
 * the drawer open until it settles.
 */
export function DrawerClose({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Medium,
  className,
  ...props
}: DrawerCloseProps) {
  return (
    <DrawerClosePart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}
