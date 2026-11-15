// SPDX-License-Identifier: Apache-2.0
import {
  PaginationEllipsis,
  PaginationItem,
  PaginationLink,
  PaginationList,
  PaginationNext,
  PaginationPrevious,
  Pagination as PaginationRoot,
} from "@ui-sdk/react/pagination";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export { createPaginationController } from "@ui-sdk/react/pagination";
export type { PaginationController } from "@ui-sdk/react/pagination";

export type PaginationProps = Omit<
  React.ComponentProps<typeof PaginationRoot>,
  "children" | "aria-label"
> & {
  /** The accessible name of the navigation landmark, such as "Pages". */
  "aria-label": string;
  /** The accessible name of the previous-page step. The default is "Previous page". */
  previousLabel?: string;
  /** The accessible name of the next-page step. The default is "Next page". */
  nextLabel?: string;
};

/**
 * A row of page links with previous and next steps, built from UI SDK
 * `Pagination`. Pass `pageCount`, or `total` and `pageSize`. With
 * `getHref`, the parts are links; without it, they are buttons and
 * `onPageChange` reports the chosen page. Long ranges collapse into
 * ellipses.
 */
export function Pagination({
  previousLabel = "Previous page",
  nextLabel = "Next page",
  className,
  ...props
}: PaginationProps) {
  return (
    <PaginationRoot
      className={mergeClassName("dui-pagination", className)}
      {...props}
    >
      <PaginationList className="dui-pagination-list">
        {({ items }) => (
          <>
            <PaginationItem>
              <PaginationPrevious
                className="dui-pagination-step"
                aria-label={previousLabel}
              >
                <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
                  <path
                    d="M10 4l-4 4 4 4"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
              </PaginationPrevious>
            </PaginationItem>
            {items.map((item) => (
              <PaginationItem key={item}>
                {typeof item === "number" ? (
                  <PaginationLink className="dui-pagination-link" page={item} />
                ) : (
                  <PaginationEllipsis className="dui-pagination-ellipsis" />
                )}
              </PaginationItem>
            ))}
            <PaginationItem>
              <PaginationNext
                className="dui-pagination-step"
                aria-label={nextLabel}
              >
                <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
                  <path
                    d="M6 4l4 4-4 4"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
              </PaginationNext>
            </PaginationItem>
          </>
        )}
      </PaginationList>
    </PaginationRoot>
  );
}
