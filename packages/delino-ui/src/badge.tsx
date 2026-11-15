// SPDX-License-Identifier: Apache-2.0
import type * as React from "react";

/** The color of a badge. */
export enum BadgeVariant {
  /** A plain label, such as a count or a category. */
  Neutral = "neutral",
  /** A highlighted label, such as "New". */
  Accent = "accent",
  /** A warning or failure, such as "Overdue". */
  Danger = "danger",
}

export type BadgeProps = React.ComponentProps<"span"> & {
  variant?: BadgeVariant;
};

/**
 * A short status or count label. It is static text with no behavior, so it
 * needs no UI SDK part. Do not use color alone to carry meaning: the text
 * must say it too.
 */
export function Badge({
  variant = BadgeVariant.Neutral,
  className,
  ...props
}: BadgeProps) {
  return (
    <span
      data-variant={variant}
      className={className ? `dui-badge ${className}` : "dui-badge"}
      {...props}
    />
  );
}
