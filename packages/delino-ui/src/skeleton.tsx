// SPDX-License-Identifier: Apache-2.0
import type * as React from "react";

export type SkeletonProps = Omit<React.ComponentProps<"span">, "children"> & {
  /**
   * The width, as a CSS length such as `120px` or `60%`. The default is
   * `100%`. With `circle`, an omitted width copies `height`, and the default
   * is `1em` when both are omitted.
   */
  width?: string;
  /**
   * The height, as a CSS length. The default is `1em`. With `circle`, an
   * omitted height copies `width`.
   */
  height?: string;
  /**
   * Draws a circle, for avatar placeholders. The circle is a square with a
   * round border, so an omitted `width` or `height` takes the other value, and
   * both default to `1em` when both are omitted. Give both only when you want
   * an ellipse.
   */
  circle?: boolean;
};

/** The diameter of a circle when neither `width` nor `height` is given. */
const DEFAULT_CIRCLE_DIAMETER = "1em";

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
  // The stylesheet defaults are `100%` by `1em`, which would stretch a circle
  // into a wide ellipse. A circle therefore resolves one diameter and uses it
  // for any omitted side. Caller `style` still wins over both.
  const diameter = circle
    ? (width ?? height ?? DEFAULT_CIRCLE_DIAMETER)
    : undefined;
  return (
    <span
      aria-hidden="true"
      data-circle={circle ? "" : undefined}
      className={className ? `dui-skeleton ${className}` : "dui-skeleton"}
      style={{
        width: width ?? diameter,
        height: height ?? diameter,
        ...style,
      }}
      {...props}
    />
  );
}
