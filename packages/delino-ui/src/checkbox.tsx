// SPDX-License-Identifier: Apache-2.0
import {
  Checkbox as CheckboxPart,
  CheckboxGroup as CheckboxGroupPart,
  CheckboxIndicator,
} from "@ui-sdk/react/checkbox";
import { renderElement } from "@ui-sdk/react/primitive";
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
  render,
  "aria-describedby": ariaDescribedBy,
  ...props
}: CheckboxProps) {
  const labelId = React.useId();
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

  // The SDK folds a render element's own description into a parent Field.
  // Give it that reference as a prop because the composed render is a callback.
  const elementDescription = React.isValidElement<React.AriaAttributes>(render)
    ? render.props["aria-describedby"]
    : undefined;
  const customRender =
    React.isValidElement<React.AriaAttributes>(render) &&
    elementDescription !== undefined
      ? React.cloneElement(render, { "aria-describedby": undefined })
      : render;
  const renderControl: CheckboxProps["render"] = customRender
    ? (controlProps, state) => {
        const control = renderElement(
          "button",
          controlProps,
          customRender,
          state,
        );
        // The SDK links non-labelable controls to the native label on mount.
        // Remove this fallback when the SDK names custom controls during SSR.
        if (
          React.isValidElement<React.AriaAttributes>(control) &&
          control.type !== React.Fragment &&
          !control.props["aria-label"]?.trim() &&
          !control.props["aria-labelledby"]?.trim()
        ) {
          return React.cloneElement(control, { "aria-labelledby": labelId });
        }
        return control;
      }
    : undefined;

  return (
    <div className="dui-checkbox-field">
      <label className="dui-checkbox-item">
        <CheckboxPart
          className={mergeClassName("dui-checkbox", className)}
          aria-describedby={
            elementDescription === undefined
              ? describedBy || undefined
              : elementDescription
          }
          render={renderControl}
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
        <span id={labelId} className="dui-checkbox-label">
          {label}
        </span>
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
