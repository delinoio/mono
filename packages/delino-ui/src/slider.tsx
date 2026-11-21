// SPDX-License-Identifier: Apache-2.0
import { Field, FieldDescription, FieldLabel } from "@ui-sdk/react/field";
import {
  Slider as SliderRoot,
  SliderControl,
  SliderIndicator,
  SliderThumb,
  SliderTrack,
  SliderValue,
} from "@ui-sdk/react/slider";
import * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type SliderProps = Omit<
  React.ComponentProps<typeof SliderRoot>,
  "children"
> & {
  /** The visible name of the slider. */
  label: React.ReactNode;
  /** Help text shown below the slider. */
  description?: React.ReactNode;
  /** Shows the formatted value, such as "40" or "20–80", next to the label. */
  showValue?: boolean;
  /**
   * The accessible names of the thumbs of a range, such as
   * `["Minimum price", "Maximum price"]`. A range has more than one value.
   * A single thumb is named by `label`, including a one-value array.
   */
  thumbLabels?: readonly string[];
};

function isEmptyArray(value: unknown): boolean {
  return Array.isArray(value) && value.length === 0;
}

/**
 * A labeled slider built from UI SDK `Field` and `Slider`. Pass a number as
 * `value` or `defaultValue` for one thumb, or an array of more than one value for a
 * range with one thumb per value. An array of one value is one thumb. Arrow keys move the focused thumb.
 *
 * An empty array in `value` or `defaultValue` counts as no value, because a
 * slider has at least one thumb. A controlled `value={[]}` therefore behaves
 * like an uncontrolled slider that starts at the UI SDK default (`min`, or 0).
 * `onValueChange` then reports a number, not an array, because the UI SDK state
 * is a number when no value is given. A later re-render with `value={[20]}`
 * shows one thumb with the value 20.
 *
 * When `value={[]}` replaces a number or a non-empty array, the slider resets
 * to that default, as if it mounted again with no value.
 */
export function Slider({
  label,
  description,
  showValue = false,
  thumbLabels,
  className,
  disabled,
  value: valueProp,
  defaultValue: defaultValueProp,
  ...props
}: SliderProps) {
  const valueIsEmpty = isEmptyArray(valueProp);
  const value = valueIsEmpty ? undefined : valueProp;
  const hasValue = value !== undefined;
  // Workaround: the UI SDK keeps its last state when `value` becomes
  // undefined, and this wrapper maps an empty array to undefined. After a
  // non-empty value is replaced by `[]`, the SDK state (for example the range
  // 20 and 80) would then differ from the one default thumb rendered here.
  // Remounting the SDK root through a new key makes it start again from `min`
  // or `defaultValue`. The key changes only when a value is replaced by an
  // empty array, so a fresh mount, `[]` to a value and an uncontrolled slider
  // keep their state. The remount drops the focus of the thumb. The state is
  // adjusted during render, which React supports for state derived from props.
  // Remove when the UI SDK resets its state when a controlled value becomes
  // undefined.
  const [resetState, setResetState] = React.useState({
    hadValue: hasValue,
    epoch: 0,
  });
  let resetEpoch = resetState.epoch;
  if (resetState.hadValue !== hasValue) {
    if (resetState.hadValue && valueIsEmpty) {
      resetEpoch += 1;
    }
    setResetState({ hadValue: hasValue, epoch: resetEpoch });
  }
  const defaultValue = isEmptyArray(defaultValueProp)
    ? undefined
    : defaultValueProp;
  const initial = value ?? defaultValue;
  const thumbCount = Array.isArray(initial) ? initial.length : 1;
  // The UI SDK treats only an array of more than one value as a range. A
  // one-value array is a single thumb that the field label names.
  const isRange = Array.isArray(initial) && initial.length > 1;
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";

  return (
    <Field className="dui-text-field" disabled={disabled}>
      <SliderRoot
        key={resetEpoch}
        className={mergeClassName("dui-slider", className)}
        disabled={disabled}
        value={value}
        defaultValue={defaultValue}
        {...props}
      >
        <div className="dui-slider-header">
          <FieldLabel className="dui-text-field-label">{label}</FieldLabel>
          {showValue ? <SliderValue className="dui-slider-value" /> : null}
        </div>
        <SliderControl className="dui-slider-control">
          <SliderTrack className="dui-slider-track">
            <SliderIndicator className="dui-slider-indicator" />
            {Array.from({ length: thumbCount }, (_, index) => (
              <SliderThumb
                // Thumbs are positional and never reorder, so the index is a
                // stable key.
                key={index}
                index={index}
                className="dui-slider-thumb"
                aria-label={isRange ? thumbLabels?.[index] : undefined}
              />
            ))}
          </SliderTrack>
        </SliderControl>
      </SliderRoot>
      {/* Workaround: the UI SDK Field predicts the description id in
          aria-describedby before the field binds on the client, so static
          markup would point at a missing element. Always render the
          description and hide it when empty (display: none from the package
          styles). Remove when the UI SDK stops predicting an id for an
          unregistered description. */}
      <FieldDescription
        className="dui-text-field-description"
        hidden={!hasDescription}
      >
        {hasDescription ? description : null}
      </FieldDescription>
    </Field>
  );
}
