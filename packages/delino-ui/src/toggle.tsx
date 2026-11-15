// SPDX-License-Identifier: Apache-2.0
import {
  Toggle as TogglePart,
  ToggleGroup as ToggleGroupPart,
} from "@ui-sdk/react/toggle";
import type * as React from "react";
import { ButtonSize } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export type ToggleProps = React.ComponentProps<typeof TogglePart> & {
  size?: ButtonSize;
};

/**
 * A button that stays pressed until it is pressed again, such as Bold in a
 * text editor, built from UI SDK `Toggle`. It reports its state with
 * `aria-pressed`. Keep its name the same in both states.
 */
export function Toggle({
  size = ButtonSize.Medium,
  className,
  ...props
}: ToggleProps) {
  return (
    <TogglePart
      data-size={size}
      className={mergeClassName("dui-toggle", className)}
      {...props}
    />
  );
}

export type ToggleGroupProps = React.ComponentProps<typeof ToggleGroupPart>;

/**
 * A set of `Toggle` buttons with one tab stop, built from UI SDK
 * `ToggleGroup`. Give each toggle a `value`; the group's value lists the
 * pressed ones. One toggle is pressed at a time unless `multiple` is set.
 * Name the group with `aria-label`.
 */
export function ToggleGroup({ className, ...props }: ToggleGroupProps) {
  return (
    <ToggleGroupPart
      className={mergeClassName("dui-toggle-group", className)}
      {...props}
    />
  );
}
