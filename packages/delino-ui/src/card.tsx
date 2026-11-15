// SPDX-License-Identifier: Apache-2.0
import type * as React from "react";

function joinClassName(base: string, className: string | undefined): string {
  return className ? `${base} ${className}` : base;
}

export type CardProps = React.ComponentProps<"section">;

/**
 * A rounded surface that groups related content. It renders a `section`;
 * name it with a `CardTitle` and `aria-labelledby`, or with `aria-label`.
 * It is a static container with no behavior, so it needs no UI SDK part.
 */
export function Card({ className, ...props }: CardProps) {
  return (
    <section className={joinClassName("dui-card", className)} {...props} />
  );
}

export type CardTitleProps = React.ComponentProps<"h3">;

/** The title of a card. A level 3 heading. */
export function CardTitle({ className, ...props }: CardTitleProps) {
  return (
    <h3 className={joinClassName("dui-card-title", className)} {...props} />
  );
}

export type CardDescriptionProps = React.ComponentProps<"p">;

/** Text that describes the card. */
export function CardDescription({ className, ...props }: CardDescriptionProps) {
  return (
    <p
      className={joinClassName("dui-card-description", className)}
      {...props}
    />
  );
}

export type CardActionsProps = React.ComponentProps<"div">;

/** The row of buttons at the bottom of the card. */
export function CardActions({ className, ...props }: CardActionsProps) {
  return (
    <div className={joinClassName("dui-card-actions", className)} {...props} />
  );
}
