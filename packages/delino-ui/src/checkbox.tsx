// SPDX-License-Identifier: Apache-2.0
import {
  Checkbox as CheckboxPart,
  CheckboxGroup as CheckboxGroupPart,
  CheckboxIndicator,
} from "@ui-sdk/react/checkbox";
import * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type { CheckedState } from "@ui-sdk/react/checkbox";

export type CheckboxProps = Omit<
  React.ComponentProps<typeof CheckboxPart>,
  "children"
> & {
  /** The visible name of the option. */
  label: React.ReactNode;
  /** More detail about the option, shown below the label. */
  description?: React.ReactNode;
};

/**
 * A labeled checkbox built from UI SDK `Checkbox`. Pressing the label
 * toggles it, and the description describes it. It shows a dash while
 * indeterminate.
 *
 * It wraps the checkbox in a native label instead of a UI SDK `Field`,
 * because a `CheckboxGroup` gives each checkbox its own id, which a field
 * label does not follow.
 */
export function Checkbox({
  label,
  description,
  className,
  "aria-describedby": ariaDescribedBy,
  ...props
}: CheckboxProps) {
  const descriptionId = React.useId();
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";
  // Keep the caller's ids first and add the description id after them, the
  // same order the UI SDK field uses. An empty caller value adds no token.
  const describedBy = [
    ariaDescribedBy,
    hasDescription ? descriptionId : undefined,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div className="dui-checkbox-field">
      <label className="dui-checkbox-item">
        <CheckboxPart
          className={mergeClassName("dui-checkbox", className)}
          aria-describedby={describedBy || undefined}
          {...props}
        >
          <CheckboxIndicator className="dui-checkbox-indicator">
            {(state) => (
              <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
                <path
                  d={state.indeterminate ? "M4 8h8" : "M3.5 8.5l3 3 6-7"}
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            )}
          </CheckboxIndicator>
        </CheckboxPart>
        <span className="dui-checkbox-label">{label}</span>
      </label>
      {hasDescription ? (
        <p id={descriptionId} className="dui-checkbox-description">
          {description}
        </p>
      ) : null}
    </div>
  );
}

export type CheckboxGroupProps = React.ComponentProps<typeof CheckboxGroupPart>;

/**
 * A list of `Checkbox` options that share one list of checked values. Name
 * the group with `aria-label` or `aria-labelledby`.
 */
export function CheckboxGroup({ className, ...props }: CheckboxGroupProps) {
  return (
    <CheckboxGroupPart
      className={mergeClassName("dui-checkbox-group", className)}
      {...props}
    />
  );
}
