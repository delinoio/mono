// SPDX-License-Identifier: Apache-2.0
import {
  Collapsible as CollapsibleRoot,
  CollapsiblePanel as CollapsiblePanelPart,
  CollapsibleTrigger as CollapsibleTriggerPart,
} from "@ui-sdk/react/collapsible";
import type * as React from "react";
import { ButtonSize, ButtonVariant } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export { createCollapsibleController } from "@ui-sdk/react/collapsible";
export type { CollapsibleController } from "@ui-sdk/react/collapsible";

export type CollapsibleProps = React.ComponentProps<typeof CollapsibleRoot>;

/**
 * Content that a button shows and hides, built from UI SDK `Collapsible`.
 * Put a `CollapsibleTrigger` and a `CollapsiblePanel` inside.
 */
export function Collapsible({ className, ...props }: CollapsibleProps) {
  return (
    <CollapsibleRoot
      className={mergeClassName("dui-collapsible", className)}
      {...props}
    />
  );
}

export type CollapsibleTriggerProps = React.ComponentProps<
  typeof CollapsibleTriggerPart
> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

/**
 * The button that shows and hides the panel. It looks like a `Button` and
 * defaults to `Secondary`.
 */
export function CollapsibleTrigger({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Medium,
  className,
  ...props
}: CollapsibleTriggerProps) {
  return (
    <CollapsibleTriggerPart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}

export type CollapsiblePanelProps = React.ComponentProps<
  typeof CollapsiblePanelPart
>;

/** The content that the trigger shows and hides. Its height animates. */
export function CollapsiblePanel({
  className,
  ...props
}: CollapsiblePanelProps) {
  return (
    <CollapsiblePanelPart
      className={mergeClassName("dui-collapsible-panel", className)}
      {...props}
    />
  );
}
