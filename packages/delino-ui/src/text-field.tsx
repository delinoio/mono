// SPDX-License-Identifier: Apache-2.0
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import { Input } from "@ui-sdk/react/input";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type TextFieldProps = React.ComponentProps<typeof Input> & {
  /** The visible name of the field. */
  label: React.ReactNode;
  /** Help text shown below the input. */
  description?: React.ReactNode;
  /**
   * An error message to show. A non-empty message marks the field as
   * invalid. Without one (`undefined`, `null`, `false` or `""`), the field
   * shows the browser's message after a failed submit.
   */
  error?: React.ReactNode;
};

/**
 * A labeled text input built from UI SDK `Field` and `Input`. The field
 * connects the label, description and error to the input.
 *
 * When this is the only critical input of a form, pass `autoFocus` so the
 * input has focus when the form is shown.
 */
export function TextField({
  label,
  description,
  error,
  className,
  disabled,
  ...props
}: TextFieldProps) {
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
      {/*
        Workaround: the UI SDK Dialog chooses its initial focus target with a
        `[autofocus], [data-autofocus]` DOM query, but React handles the
        `autoFocus` prop by calling `focus()` and renders no `autofocus`
        attribute. Without `data-autofocus`, a Dialog focuses its first
        tabbable element (usually the close button) instead of this input.
        Scope: only when `autoFocus` is true; a caller-supplied
        `data-autofocus` wins. Remove when the UI SDK reads the React
        `autoFocus` prop itself.
      */}
      <Input
        className={mergeClassName("dui-text-field-input", className)}
        disabled={disabled}
        data-autofocus={props.autoFocus ? "" : undefined}
        {...props}
      />
      {!hasDescription ? null : (
        <FieldDescription className="dui-text-field-description">
          {description}
        </FieldDescription>
      )}
      {hasError ? (
        <FieldError className="dui-text-field-error">{error}</FieldError>
      ) : (
        <FieldError className="dui-text-field-error" />
      )}
    </Field>
  );
}
