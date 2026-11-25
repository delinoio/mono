// SPDX-License-Identifier: Apache-2.0
import {
  MeterIndicator,
  MeterLabel,
  Meter as MeterRoot,
  MeterTrack,
  MeterValue,
} from "@ui-sdk/react/meter";
import * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type MeterProps = Omit<
  React.ComponentProps<typeof MeterRoot>,
  "children"
> & {
  /** The visible name of the measurement, such as "Storage used". */
  label: React.ReactNode;
  /** Shows the formatted value, such as "40%", next to the label. */
  showValue?: boolean;
};

/**
 * A bar that shows a measurement within a known range, such as storage used
 * or a password's strength, built from UI SDK `Meter`. For the progress of
 * a task, use `Progress` instead.
 */
export function Meter({
  label,
  showValue = false,
  className,
  "aria-label": ariaLabel,
  "aria-labelledby": ariaLabelledBy,
  ...props
}: MeterProps) {
  // The UI SDK registers the Meter label after mount. Supply its ID on the
  // first render for the visible-label fallback. An empty aria-labelledby keeps
  // the SDK from overriding an explicit aria-label after mount. Remove these
  // overrides when the SDK names the bar correctly in both cases.
  const labelId = React.useId();
  const callerLabelledBy = ariaLabelledBy?.trim() ? ariaLabelledBy : undefined;
  return (
    <MeterRoot
      className={mergeClassName("dui-progress", className)}
      aria-label={ariaLabel}
      aria-labelledby={callerLabelledBy ?? (ariaLabel?.trim() ? "" : labelId)}
      {...props}
    >
      <div className="dui-progress-header">
        <MeterLabel id={labelId} className="dui-progress-label">
          {label}
        </MeterLabel>
        {showValue ? <MeterValue className="dui-progress-value" /> : null}
      </div>
      <MeterTrack className="dui-progress-track">
        <MeterIndicator className="dui-progress-indicator" />
      </MeterTrack>
    </MeterRoot>
  );
}
