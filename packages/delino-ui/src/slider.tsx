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
   * A range thumb with no entry, or an empty entry, is named by `label` and
   * its one-based position, such as "Price 1 of 2". A single thumb is named by
   * `label`, including a one-value array.
   */
  thumbLabels?: readonly string[];
};

type SliderValueProp = SliderProps["value"];

function thumbCount(value: SliderValueProp): number {
  return Array.isArray(value) ? value.length : 1;
}

function isRangeValue(value: SliderValueProp): boolean {
  return thumbCount(value) > 1;
}

// The SDK adapter chooses its Field bindings from value ?? defaultValue.
// Keep a mode-compatible reset default when control is released. If the
// original default cannot represent the mode, use its first controlled value.
// Remove this fallback when the SDK adapter chooses bindings from live state.
function compatibleResetDefault(
  defaultValue: SliderValueProp,
  value: SliderValueProp,
): SliderValueProp {
  if (
    value !== undefined &&
    isRangeValue(value) !== isRangeValue(defaultValue)
  ) {
    return value;
  }
  return defaultValue;
}

/**
 * A labeled slider built from UI SDK `Field` and `Slider`. A number or
 * one-value array gives one thumb. A longer array gives one thumb per value.
 * Compatible controlled updates preserve the mounted mode. Omit `value`
 * to release control while keeping the current values.
 *
 * An empty `defaultValue` is treated as absent. Passing `value={[]}` after
 * a controlled value resets the slider to the current default (or `min`).
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
  onValueChange,
  "aria-label": ariaLabel,
  "aria-labelledby": ariaLabelledBy,
  ...props
}: SliderProps) {
  const valueIsEmpty = thumbCount(valueProp) === 0;
  const requestedValue = valueIsEmpty ? undefined : valueProp;
  const currentDefault =
    thumbCount(defaultValueProp) === 0 ? undefined : defaultValueProp;
  const [configuration, setConfiguration] = React.useState(() => ({
    epoch: 0,
    controlled:
      requestedValue === undefined
        ? undefined
        : { initial: requestedValue, value: requestedValue },
    resetDefault: currentDefault,
    defaultProp: currentDefault,
  }));

  let { controlled, resetDefault } = configuration;
  if (valueIsEmpty && controlled !== undefined) {
    // The SDK keeps the current value when its value prop becomes undefined.
    // Remount only for the explicit empty-array reset. Remove this workaround
    // when the SDK can reset to the supplied default without a remount.
    setConfiguration({
      epoch: configuration.epoch + 1,
      controlled: undefined,
      resetDefault: currentDefault,
      defaultProp: currentDefault,
    });
  } else {
    if (requestedValue !== undefined) {
      if (controlled === undefined) {
        controlled = { initial: requestedValue, value: requestedValue };
      } else if (
        requestedValue !== controlled.value &&
        isRangeValue(requestedValue) === isRangeValue(controlled.value)
      ) {
        controlled = { ...controlled, value: requestedValue };
      }
    } else if (
      currentDefault !== undefined &&
      currentDefault !== configuration.defaultProp &&
      thumbCount(currentDefault) ===
        thumbCount(compatibleResetDefault(resetDefault, controlled?.initial))
    ) {
      resetDefault = currentDefault;
    }

    if (
      controlled !== configuration.controlled ||
      resetDefault !== configuration.resetDefault ||
      currentDefault !== configuration.defaultProp
    ) {
      setConfiguration({
        epoch: configuration.epoch,
        controlled,
        resetDefault,
        defaultProp: currentDefault,
      });
    }
  }

  const value = requestedValue === undefined ? undefined : controlled?.value;
  const defaultValue =
    value === undefined
      ? compatibleResetDefault(resetDefault, controlled?.initial)
      : resetDefault;
  const isRange = isRangeValue(value ?? defaultValue);
  // A range thumb without an explicit name is named by the visible label and
  // its position. The names reference the rendered label by id, so a React node
  // label works and the names stay correct on the server and the client.
  const labelId = React.useId();
  const positionIdPrefix = React.useId();
  const hasThumbLabel = (index: number) => {
    const thumbLabel = thumbLabels?.[index];
    return typeof thumbLabel === "string" && thumbLabel !== "";
  };
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";

  return (
    <Field className="dui-text-field" disabled={disabled}>
      <SliderRoot
        key={configuration.epoch}
        className={mergeClassName("dui-slider", className)}
        disabled={disabled}
        value={value}
        defaultValue={defaultValue}
        onValueChange={
          isRange && value === undefined
            ? (next, details) => {
                if (thumbCount(next) < 2) {
                  details.cancel();
                  return;
                }
                onValueChange?.(next, details);
              }
            : onValueChange
        }
        aria-label={isRange && !ariaLabel?.trim() ? undefined : ariaLabel}
        // An explicit empty reference suppresses the SDK's predicted Field
        // label ID when a caller names the group with aria-label. Remove this
        // override when the SDK respects aria-label on its own.
        aria-labelledby={
          isRange
            ? ariaLabelledBy?.trim()
              ? ariaLabelledBy
              : ariaLabel?.trim()
                ? ""
                : labelId
            : ariaLabelledBy
        }
        {...props}
      >
        {(state) => {
          const count = isRange ? thumbCount(state.value) : 1;
          return (
            <>
              <div className="dui-slider-header">
                <FieldLabel
                  className="dui-text-field-label"
                  id={isRange ? labelId : undefined}
                >
                  {label}
                </FieldLabel>
                {showValue ? (
                  <SliderValue className="dui-slider-value" />
                ) : null}
              </div>
              <SliderControl className="dui-slider-control">
                <SliderTrack className="dui-slider-track">
                  <SliderIndicator className="dui-slider-indicator" />
                  {Array.from({ length: count }, (_, index) => (
                    <SliderThumb
                      // Thumbs are positional and never reorder, so the index
                      // is a stable key.
                      key={index}
                      index={index}
                      className="dui-slider-thumb"
                      // An explicit name keeps `aria-label`. Without one, the
                      // thumb is named by the label and the hidden position
                      // text below; `aria-labelledby` would override
                      // `aria-label`, so the two are never set together.
                      aria-label={
                        isRange && hasThumbLabel(index)
                          ? thumbLabels?.[index]
                          : undefined
                      }
                      aria-labelledby={
                        isRange && !hasThumbLabel(index)
                          ? `${labelId} ${positionIdPrefix}-${index}`
                          : undefined
                      }
                    />
                  ))}
                </SliderTrack>
              </SliderControl>
              {isRange
                ? Array.from({ length: count }, (_, index) =>
                    hasThumbLabel(index) ? null : (
                      // Hidden text that only `aria-labelledby` of a thumb
                      // reads.
                      <span
                        key={index}
                        id={`${positionIdPrefix}-${index}`}
                        hidden
                      >{`${index + 1} of ${count}`}</span>
                    ),
                  )
                : null}
            </>
          );
        }}
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
