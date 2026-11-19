// SPDX-License-Identifier: Apache-2.0
import type * as React from "react";

export type SkeletonProps = Omit<React.ComponentProps<"span">, "children"> & {
  /**
   * The width, as a CSS length such as `120px` or `60%`. The default is
   * `100%`. With `circle`, an omitted width equals the rendered `height`, and
   * the default is `1em` when both are omitted.
   */
  width?: string;
  /**
   * The height, as a CSS length. The default is `1em`. With `circle`, an
   * omitted height equals the rendered `width`, also for a percentage width.
   */
  height?: string;
  /**
   * Draws a circle, for avatar placeholders. The circle is a square with a
   * round border, so an omitted `width` or `height` follows the other side
   * through a square `aspect-ratio`, and both default to `1em` when both are
   * omitted. Give both only when you want an ellipse. A circle keeps its size
   * and is not stretched by a flex or grid container; the stylesheet sets
   * `align-self: flex-start` and `justify-self: flex-start` on it.
   */
  circle?: boolean;
};

/** The diameter of a circle when neither `width` nor `height` is given. */
const DEFAULT_CIRCLE_DIAMETER = "1em";

/** Lets `aspect-ratio` derive a circle's omitted side from the given one. */
const AUTO_SIZE = "auto";
const SQUARE_ASPECT_RATIO = "1 / 1";

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
  // into a wide ellipse. Copying one side to the other is not enough, because
  // a percentage width resolves against the container width and a percentage
  // height against its height, which is often `auto`. A circle with one side
  // therefore keeps that side and takes the other from `aspect-ratio`; the
  // other side is set to `auto` so the stylesheet default does not apply.
  // Caller `style` still wins over all of these.
  const hasWidth = width !== undefined;
  const hasHeight = height !== undefined;
  const circleStyle: React.CSSProperties = !circle
    ? {}
    : hasWidth && hasHeight
      ? {}
      : hasWidth || hasHeight
        ? {
            width: hasWidth ? width : AUTO_SIZE,
            height: hasHeight ? height : AUTO_SIZE,
            aspectRatio: SQUARE_ASPECT_RATIO,
          }
        : { width: DEFAULT_CIRCLE_DIAMETER, height: DEFAULT_CIRCLE_DIAMETER };
  return (
    <span
      aria-hidden="true"
      data-circle={circle ? "" : undefined}
      className={className ? `dui-skeleton ${className}` : "dui-skeleton"}
      style={{
        width,
        height,
        ...circleStyle,
        ...style,
      }}
      {...props}
    />
  );
}
