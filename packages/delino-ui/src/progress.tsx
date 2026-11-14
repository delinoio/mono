// SPDX-License-Identifier: Apache-2.0
import {
  Progress as ProgressRoot,
  ProgressIndicator,
  ProgressLabel,
  ProgressTrack,
  ProgressValue,
} from "@ui-sdk/react/progress";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type ProgressProps = Omit<
  React.ComponentProps<typeof ProgressRoot>,
  "children"
> & {
  /** The visible name of the task. */
  label: React.ReactNode;
  /** Shows the formatted value, such as "40%", next to the label. */
  showValue?: boolean;
};

/**
 * A bar that shows how far a task has come, built from UI SDK `Progress`.
 * A `null` value shows an indeterminate bar for a task of unknown length.
 */
export function Progress({
  label,
  showValue = false,
  className,
  ...props
}: ProgressProps) {
  return (
    <ProgressRoot
      className={mergeClassName("dui-progress", className)}
      {...props}
    >
      <div className="dui-progress-header">
        <ProgressLabel className="dui-progress-label">{label}</ProgressLabel>
        {showValue ? <ProgressValue className="dui-progress-value" /> : null}
      </div>
      <ProgressTrack className="dui-progress-track">
        <ProgressIndicator className="dui-progress-indicator" />
      </ProgressTrack>
    </ProgressRoot>
  );
}
