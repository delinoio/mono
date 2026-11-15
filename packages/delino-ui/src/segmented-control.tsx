// SPDX-License-Identifier: Apache-2.0
import {
  SegmentedControlIndicator,
  SegmentedControlItem as SegmentedControlItemPart,
  SegmentedControl as SegmentedControlRoot,
} from "@ui-sdk/react/segmented-control";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type SegmentedControlProps = Omit<
  React.ComponentProps<typeof SegmentedControlRoot>,
  "children"
> & {
  /** `SegmentedControlItem` elements. */
  children?: React.ReactNode;
};

/**
 * A row of mutually exclusive choices with a highlight that slides to the
 * checked one, built from UI SDK `SegmentedControl`. Name it with
 * `aria-label`. Arrow keys move between items.
 */
export function SegmentedControl({
  className,
  children,
  ...props
}: SegmentedControlProps) {
  return (
    <SegmentedControlRoot
      className={mergeClassName("dui-segmented-control", className)}
      {...props}
    >
      <SegmentedControlIndicator className="dui-segmented-control-indicator" />
      {children}
    </SegmentedControlRoot>
  );
}

export type SegmentedControlItemProps = React.ComponentProps<
  typeof SegmentedControlItemPart
>;

/** One choice. Give each item a unique `value`. */
export function SegmentedControlItem({
  className,
  ...props
}: SegmentedControlItemProps) {
  return (
    <SegmentedControlItemPart
      className={mergeClassName("dui-segmented-control-item", className)}
      {...props}
    />
  );
}
