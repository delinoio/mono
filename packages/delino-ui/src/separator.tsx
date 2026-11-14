// SPDX-License-Identifier: Apache-2.0
import { Separator as SeparatorPart } from "@ui-sdk/react/separator";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type SeparatorProps = React.ComponentProps<typeof SeparatorPart>;

/**
 * A thin line between two groups of content, built from UI SDK `Separator`.
 * Set `orientation="vertical"` for a line between items in a row.
 */
export function Separator({ className, ...props }: SeparatorProps) {
  return (
    <SeparatorPart
      className={mergeClassName("dui-separator", className)}
      {...props}
    />
  );
}
