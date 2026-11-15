// SPDX-License-Identifier: Apache-2.0
import {
  Popover as PopoverRoot,
  PopoverClose as PopoverClosePart,
  PopoverContent as PopoverContentPart,
  PopoverDescription as PopoverDescriptionPart,
  PopoverTitle as PopoverTitlePart,
  PopoverTrigger as PopoverTriggerPart,
} from "@ui-sdk/react/popover";
import type * as React from "react";
import { ButtonSize, ButtonVariant } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export { createPopoverController } from "@ui-sdk/react/popover";
export type { PopoverController } from "@ui-sdk/react/popover";

/**
 * The props of `Popover`. `dismissible` is left out because the popover
 * content is a dialog, and every dialog in Delino apps must close with the
 * Escape key.
 */
export type PopoverProps = Omit<
  React.ComponentProps<typeof PopoverRoot>,
  "dismissible"
>;

/**
 * A panel of interactive content anchored to its trigger, built from UI SDK
 * `Popover`. It closes with Escape, a press outside or focus leaving it.
 */
export function Popover(props: PopoverProps) {
  return <PopoverRoot {...props} />;
}

type ButtonStyleProps = {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

export type PopoverTriggerProps = React.ComponentProps<
  typeof PopoverTriggerPart
> &
  ButtonStyleProps;

/**
 * The button that opens the popover. It looks like a `Button` and defaults
 * to `Secondary`.
 */
export function PopoverTrigger({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Medium,
  className,
  ...props
}: PopoverTriggerProps) {
  return (
    <PopoverTriggerPart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}

export type PopoverContentProps = React.ComponentProps<
  typeof PopoverContentPart
>;

/** The popover panel. It opens 8px from the trigger by default. */
export function PopoverContent({
  className,
  sideOffset = 8,
  ...props
}: PopoverContentProps) {
  return (
    <PopoverContentPart
      className={mergeClassName("dui-popover", className)}
      sideOffset={sideOffset}
      {...props}
    />
  );
}

export type PopoverTitleProps = React.ComponentProps<typeof PopoverTitlePart>;

/** The title that names the popover. A level 2 heading. */
export function PopoverTitle({ className, ...props }: PopoverTitleProps) {
  return (
    <PopoverTitlePart
      className={mergeClassName("dui-popover-title", className)}
      {...props}
    />
  );
}

export type PopoverDescriptionProps = React.ComponentProps<
  typeof PopoverDescriptionPart
>;

/** Text that describes the popover. */
export function PopoverDescription({
  className,
  ...props
}: PopoverDescriptionProps) {
  return (
    <PopoverDescriptionPart
      className={mergeClassName("dui-popover-description", className)}
      {...props}
    />
  );
}

export type PopoverCloseProps = React.ComponentProps<typeof PopoverClosePart> &
  ButtonStyleProps;

/** A button that closes the popover. It defaults to `Secondary`. */
export function PopoverClose({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Small,
  className,
  ...props
}: PopoverCloseProps) {
  return (
    <PopoverClosePart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}
