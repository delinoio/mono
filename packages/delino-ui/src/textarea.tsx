// SPDX-License-Identifier: Apache-2.0
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import { Textarea as TextareaPart } from "@ui-sdk/react/textarea";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type TextareaProps = React.ComponentProps<typeof TextareaPart> & {
  /** The visible name of the field. */
  label: React.ReactNode;
  /** Help text shown below the textarea. */
  description?: React.ReactNode;
  /**
   * An error message to show. A non-empty message marks the field as
   * invalid. Without one, the field shows the browser's message after a
   * failed submit.
   */
  error?: React.ReactNode;
};

/**
 * A labeled multi-line text input built from UI SDK `Field` and `Textarea`.
 * Pass `autoResize` to grow it with its text, up to its CSS `max-height`.
 *
 * When this is the only critical input of a form, pass `autoFocus` so the
 * textarea has focus when the form is shown.
 */
export function Textarea({
  label,
  description,
  error,
  className,
  disabled,
  ...props
}: TextareaProps) {
  const hasError =
    error !== undefined && error !== null && error !== false && error !== "";

  return (
    <Field
      className="dui-text-field"
      disabled={disabled}
      invalid={hasError ? true : undefined}
    >
      <FieldLabel className="dui-text-field-label">{label}</FieldLabel>
      <TextareaPart
        className={mergeClassName("dui-textarea", className)}
        disabled={disabled}
        {...props}
      />
      {description === undefined ? null : (
        <FieldDescription className="dui-text-field-description">
          {description}
        </FieldDescription>
      )}
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
