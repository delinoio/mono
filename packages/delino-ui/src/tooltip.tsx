// SPDX-License-Identifier: Apache-2.0
import {
  Tooltip as TooltipRoot,
  TooltipContent as TooltipContentPart,
  TooltipTrigger as TooltipTriggerPart,
} from "@ui-sdk/react/tooltip";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type TooltipProps = React.ComponentProps<typeof TooltipRoot>;

/**
 * A short plain-text hint that shows while the pointer rests on its trigger
 * or keyboard focus reaches it. It closes with the Escape key. For
 * interactive content, use a dialog instead.
 */
export function Tooltip(props: TooltipProps) {
  return <TooltipRoot {...props} />;
}

export type TooltipTriggerProps = React.ComponentProps<
  typeof TooltipTriggerPart
>;

/**
 * The element the tooltip describes. It renders a button; pass `render` to
 * use another focusable element, such as a `Button`.
 */
export function TooltipTrigger(props: TooltipTriggerProps) {
  return <TooltipTriggerPart {...props} />;
}

export type TooltipContentProps = React.ComponentProps<
  typeof TooltipContentPart
>;

/** The hint text. It opens 8px from the trigger by default. */
export function TooltipContent({
  className,
  sideOffset = 8,
  ...props
}: TooltipContentProps) {
  return (
    <TooltipContentPart
      className={mergeClassName("dui-tooltip", className)}
      sideOffset={sideOffset}
      {...props}
    />
  );
}
