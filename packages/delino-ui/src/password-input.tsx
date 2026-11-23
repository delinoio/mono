// SPDX-License-Identifier: Apache-2.0
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import {
  PasswordInputControl,
  PasswordInput as PasswordInputRoot,
  PasswordInputToggle,
} from "@ui-sdk/react/password-input";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";
import { useForwardedAutoFocus } from "./dialog-context.ts";

export type PasswordInputProps = Omit<
  React.ComponentProps<typeof PasswordInputRoot>,
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
  /**
   * The accessible name of the show/hide button. It stays the same while
   * the password shows; `aria-pressed` reports the state. The default is
   * "Show password".
   */
  toggleLabel?: string;
  /** Shown while the input is empty. */
  placeholder?: string;
  /** Focuses the input when it is shown. */
  autoFocus?: boolean;
};

/**
 * A labeled password field with a button that shows and hides the
 * password, built from UI SDK `Field` and `PasswordInput`. Pass
 * `autoComplete="current-password"` for sign-in forms and `"new-password"`
 * for sign-up and change forms.
 *
 * When this is the only critical input of a form, pass `autoFocus`.
 */
export function PasswordInput({
  label,
  description,
  error,
  toggleLabel = "Show password",
  placeholder,
  autoFocus,
  className,
  disabled,
  ...props
}: PasswordInputProps) {
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
      <PasswordInputRoot
        className={mergeClassName("dui-password-input", className)}
        disabled={disabled}
        {...rootProps}
      >
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
        <PasswordInputControl
          className="dui-password-input-control"
          placeholder={placeholder}
          autoFocus={forwardedAutoFocus}
          aria-describedby={ariaDescribedBy}
          aria-label={ariaLabel}
          aria-labelledby={ariaLabelledBy}
          data-autofocus={autoFocus ? "" : undefined}
        />
        <PasswordInputToggle
          className="dui-password-input-toggle"
          aria-label={toggleLabel}
        >
          {(state) =>
            state.visible ? (
              <svg aria-hidden="true" viewBox="0 0 20 20" fill="none">
                <path
                  d="M3 3l14 14M8.6 8.6a2 2 0 002.8 2.8M6.2 6.2C4.3 7.3 3 9 2.5 10c1.2 2.6 4 5 7.5 5 1.4 0 2.7-.4 3.8-1M9 5.1c.3 0 .7-.1 1-.1 3.5 0 6.3 2.4 7.5 5-.4.9-1 1.8-1.8 2.6"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            ) : (
              <svg aria-hidden="true" viewBox="0 0 20 20" fill="none">
                <path
                  d="M2.5 10c1.2-2.6 4-5 7.5-5s6.3 2.4 7.5 5c-1.2 2.6-4 5-7.5 5s-6.3-2.4-7.5-5z"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinejoin="round"
                />
                <circle
                  cx="10"
                  cy="10"
                  r="2.5"
                  stroke="currentColor"
                  strokeWidth="1.5"
                />
              </svg>
            )
          }
        </PasswordInputToggle>
      </PasswordInputRoot>
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
