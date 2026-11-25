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
  "aria-label": ariaLabel,
  "aria-labelledby": ariaLabelledBy,
  ...props
}: ProgressProps) {
  // The UI SDK registers the Progress label after mount. Supply its ID on the
  // first render for the visible-label fallback. An empty aria-labelledby keeps
  // the SDK from overriding an explicit aria-label after mount. Remove these
  // overrides when the SDK names the bar correctly in both cases.
  const labelId = React.useId();
  const callerLabelledBy = ariaLabelledBy?.trim() ? ariaLabelledBy : undefined;
  return (
    <ProgressRoot
      className={mergeClassName("dui-progress", className)}
      aria-label={ariaLabel}
      aria-labelledby={callerLabelledBy ?? (ariaLabel?.trim() ? "" : labelId)}
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
