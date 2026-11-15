// SPDX-License-Identifier: Apache-2.0
import {
  MeterIndicator,
  MeterLabel,
  Meter as MeterRoot,
  MeterTrack,
  MeterValue,
} from "@ui-sdk/react/meter";
import type * as React from "react";
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
  ...props
}: MeterProps) {
  return (
    <MeterRoot className={mergeClassName("dui-progress", className)} {...props}>
      <div className="dui-progress-header">
        <MeterLabel className="dui-progress-label">{label}</MeterLabel>
        {showValue ? <MeterValue className="dui-progress-value" /> : null}
      </div>
      <MeterTrack className="dui-progress-track">
        <MeterIndicator className="dui-progress-indicator" />
      </MeterTrack>
    </MeterRoot>
  );
}
