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
import { useForwardedAutoFocus } from "./dialog-context.ts";

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
  // A caller `data-autofocus` would be spread onto the non-tabbable root
  // `<div>`. The UI SDK Dialog takes the first `[data-autofocus]` match in
  // document order and does not try later matches, so a root match comes
  // before the control and makes the Dialog fall back to its first tabbable
  // element. Drop the attribute so only the control, which sets it from
  // `autoFocus`, can be the initial focus target. The prop types do not
  // declare it, so the cast reaches the key. Remove this when the UI SDK
  // Dialog skips non-tabbable `[data-autofocus]` matches.
  //
  // Caller `aria-describedby`, `aria-label` and `aria-labelledby` go to the
  // control too: the root `<div>` has no role, so they would name or describe
  // nothing there. The UI SDK merges the caller `aria-describedby` ahead of
  // the description and error ids on the control. All other caller props stay
  // on the root.
  const {
    "data-autofocus": _dataAutofocus,
    "aria-describedby": ariaDescribedBy,
    "aria-label": ariaLabel,
    "aria-labelledby": ariaLabelledBy,
    ...rootProps
  } = props as typeof props & {
    "data-autofocus"?: unknown;
  };
  const forwardedAutoFocus = useForwardedAutoFocus(autoFocus);
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
        {...rootProps}
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
            the root `<div>` is not tabbable, so a caller `data-autofocus` is dropped
            from the root props above.
            Exception: the caller ARIA props are not dropped but routed to the
            control (see above), because the root has no role.
            Scope: only when `autoFocus` is true. Remove when the UI SDK reads the
            React `autoFocus` prop itself.
          */}
          <NumberInputControl
            className="dui-number-input-control"
            placeholder={placeholder}
            autoFocus={forwardedAutoFocus}
            aria-describedby={ariaDescribedBy}
            aria-label={ariaLabel}
            aria-labelledby={ariaLabelledBy}
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
      {hasError ? (
        <FieldError className="dui-text-field-error">{error}</FieldError>
      ) : (
        <FieldError className="dui-text-field-error" />
      )}
    </Field>
  );
}
