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
import { renderFieldLabel } from "./field-label-render.tsx";
import { useFieldAutoFocus } from "./panel-autofocus.ts";

export type TextareaProps = React.ComponentProps<typeof TextareaPart> & {
  /** The visible name of the field. */
  label: React.ReactNode;
  /** Help text shown below the textarea. */
  description?: React.ReactNode;
  /**
   * A non-empty error message marks an enabled field as invalid. Without
   * one, an enabled field shows the browser's message after a failed submit.
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
  const { autoFocus, marker } = useFieldAutoFocus(props.autoFocus, disabled);
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
      invalid={hasError && !disabled ? true : undefined}
    >
      <FieldLabel
        className="dui-text-field-label"
        render={renderFieldLabel(props.id, props.render != null)}
      >
        {label}
      </FieldLabel>
      <TextareaPart
        className={mergeClassName("dui-textarea", className)}
        disabled={disabled}
        data-autofocus={marker}
        {...props}
        autoFocus={autoFocus}
      />
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
