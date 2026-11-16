// SPDX-License-Identifier: Apache-2.0
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import {
  NumberInputControl,
  NumberInputDecrement,
  NumberInputGroup,
  NumberInputIncrement,
  NumberInput as NumberInputRoot,
} from "@ui-sdk/react/number-input";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type NumberInputProps = Omit<
  React.ComponentProps<typeof NumberInputRoot>,
  "children"
> & {
  /** The visible name of the field. */
  label: React.ReactNode;
  /** Help text shown below the input. */
  description?: React.ReactNode;
  /**
   * An error message to show. A non-empty message marks the field as
   * invalid. Without one, the field shows the browser's message after a
   * failed submit.
   */
  error?: React.ReactNode;
  /** The accessible name of the minus button. The default is "Decrease". */
  decrementLabel?: string;
  /** The accessible name of the plus button. The default is "Increase". */
  incrementLabel?: string;
  /** Shown while the input is empty. */
  placeholder?: string;
  /** Focuses the input when it is shown. */
  autoFocus?: boolean;
};

/**
 * A labeled number field with minus and plus buttons, built from UI SDK
 * `Field` and `NumberInput`. Typing, arrow keys and the buttons change the
 * value; Enter or leaving the input clamps it to `min` and `max` and snaps
 * it to `step`. An empty input has the value `null`.
 */
export function NumberInput({
  label,
  description,
  error,
  decrementLabel = "Decrease",
  incrementLabel = "Increase",
  placeholder,
  autoFocus,
  className,
  disabled,
  ...props
}: NumberInputProps) {
  const hasError =
    error !== undefined && error !== null && error !== false && error !== "";
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";

  return (
    <Field
      className="dui-text-field"
      disabled={disabled}
      invalid={hasError ? true : undefined}
    >
      <FieldLabel className="dui-text-field-label">{label}</FieldLabel>
      <NumberInputRoot
        className={mergeClassName("dui-number-input", className)}
        disabled={disabled}
        {...props}
      >
        <NumberInputGroup className="dui-number-input-group">
          <NumberInputDecrement
            className="dui-number-input-button"
            aria-label={decrementLabel}
          >
            <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
              <path
                d="M3.5 8h9"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          </NumberInputDecrement>
          {/*
            Workaround: the UI SDK Dialog chooses its initial focus target with a
            `[autofocus], [data-autofocus]` DOM query, but React handles the
            `autoFocus` prop by calling `focus()` and renders no `autofocus`
            attribute. Without `data-autofocus` on the control `<input>`, a Dialog
            focuses its first tabbable element (usually the close button) instead
            of this input. The attribute must be on the control, not the root:
            caller props are spread onto the root `<div>`, which is not tabbable.
            Scope: only when `autoFocus` is true. Remove when the UI SDK reads the
            React `autoFocus` prop itself.
          */}
          <NumberInputControl
            className="dui-number-input-control"
            placeholder={placeholder}
            autoFocus={autoFocus}
            data-autofocus={autoFocus ? "" : undefined}
          />
          <NumberInputIncrement
            className="dui-number-input-button"
            aria-label={incrementLabel}
          >
            <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
              <path
                d="M3.5 8h9M8 3.5v9"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          </NumberInputIncrement>
        </NumberInputGroup>
      </NumberInputRoot>
      {hasDescription ? (
        <FieldDescription className="dui-text-field-description">
          {description}
        </FieldDescription>
      ) : null}
      {hasError ? (
        <FieldError className="dui-text-field-error" match>
          {error}
        </FieldError>
      ) : (
        <FieldError className="dui-text-field-error" />
      )}
    </Field>
  );
}
