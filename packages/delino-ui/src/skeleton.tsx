// SPDX-License-Identifier: Apache-2.0
import type * as React from "react";

export type SkeletonProps = Omit<React.ComponentProps<"span">, "children"> & {
  /** The width, as a CSS length such as `120px` or `60%`. The default is `100%`. */
  width?: string;
  /** The height, as a CSS length. The default is `1em`. */
  height?: string;
  /** Draws a circle, for avatar placeholders. */
  circle?: boolean;
};

/**
 * A pulsing placeholder that holds the space of content while it loads. It
 * is hidden from screen readers; mark the loading region with `aria-busy`
 * so they know content is on its way.
 */
export function Skeleton({
  width,
  height,
  circle = false,
  className,
  style,
  ...props
}: SkeletonProps) {
  return (
    <span
      aria-hidden="true"
      data-circle={circle ? "" : undefined}
      className={className ? `dui-skeleton ${className}` : "dui-skeleton"}
      style={{ width, height, ...style }}
      {...props}
    />
  );
}
