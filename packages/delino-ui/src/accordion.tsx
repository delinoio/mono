// SPDX-License-Identifier: Apache-2.0
import {
  Accordion as AccordionRoot,
  AccordionHeader,
  AccordionItem as AccordionItemPart,
  AccordionPanel as AccordionPanelPart,
  AccordionTrigger,
} from "@ui-sdk/react/accordion";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export { createAccordionController } from "@ui-sdk/react/accordion";
export type { AccordionController } from "@ui-sdk/react/accordion";

export type AccordionProps = React.ComponentProps<typeof AccordionRoot>;

/**
 * Sections that open and close from their headings, built from UI SDK
 * `Accordion`. One section opens at a time unless `multiple` is set. The
 * value is always an array of open item values.
 */
export function Accordion({ className, ...props }: AccordionProps) {
  return (
    <AccordionRoot
      className={mergeClassName("dui-accordion", className)}
      {...props}
    />
  );
}

export type AccordionItemProps = Omit<
  React.ComponentProps<typeof AccordionItemPart>,
  "title" | "children"
> & {
  /** The heading text. Its button opens and closes the panel. */
  title: React.ReactNode;
  /** The panel content. */
  children?: React.ReactNode;
  /**
   * The heading element that holds the trigger. Use a level that fits the
   * surrounding document. The default is `h3`.
   */
  headingLevel?: AccordionHeadingLevel;
};

/** The heading levels that `AccordionItem` can render. */
export enum AccordionHeadingLevel {
  H2 = "h2",
  H3 = "h3",
  H4 = "h4",
  H5 = "h5",
  H6 = "h6",
}

/**
 * One section: a heading with a trigger button, and the panel that the
 * button opens. Give each item a unique `value`.
 */
export function AccordionItem({
  title,
  headingLevel: Heading = AccordionHeadingLevel.H3,
  className,
  children,
  ...props
}: AccordionItemProps) {
  return (
    <AccordionItemPart
      className={mergeClassName("dui-accordion-item", className)}
      {...props}
    >
      <AccordionHeader className="dui-accordion-header" render={<Heading />}>
        <AccordionTrigger className="dui-accordion-trigger">
          <span className="dui-accordion-title">{title}</span>
          <svg
            aria-hidden="true"
            className="dui-accordion-icon"
            viewBox="0 0 16 16"
            fill="none"
          >
            <path
              d="M4 6l4 4 4-4"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </AccordionTrigger>
      </AccordionHeader>
      <AccordionPanelPart className="dui-accordion-panel">
        <div className="dui-accordion-panel-content">{children}</div>
      </AccordionPanelPart>
    </AccordionItemPart>
  );
}
