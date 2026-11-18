// SPDX-License-Identifier: Apache-2.0
import {
  Progress as ProgressRoot,
  ProgressIndicator,
  ProgressLabel,
  ProgressTrack,
  ProgressValue,
} from "@ui-sdk/react/progress";
import * as React from "react";
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
  "aria-labelledby": ariaLabelledBy,
  ...props
}: ProgressProps) {
  // Workaround: the UI SDK registers the gauge label only after mount, so
  // static (server-rendered) markup has a label id that nothing references and
  // the bar has no accessible name. This id gives the root a reference in the
  // first render. Scope: `Progress` only. Remove it when the UI SDK registers the
  // gauge label before it renders the root.
  const labelId = React.useId();
  return (
    <ProgressRoot
      className={mergeClassName("dui-progress", className)}
      aria-labelledby={ariaLabelledBy || labelId}
      {...props}
    >
      <div className="dui-progress-header">
        <ProgressLabel id={labelId} className="dui-progress-label">
          {label}
        </ProgressLabel>
        {showValue ? <ProgressValue className="dui-progress-value" /> : null}
      </div>
      <ProgressTrack className="dui-progress-track">
        <ProgressIndicator className="dui-progress-indicator" />
      </ProgressTrack>
    </ProgressRoot>
  );
}
