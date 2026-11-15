// SPDX-License-Identifier: Apache-2.0
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import {
  Radio,
  RadioGroup as RadioGroupPart,
  RadioIndicator,
} from "@ui-sdk/react/radio";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type RadioGroupProps = React.ComponentProps<typeof RadioGroupPart> & {
  /** The visible name of the group. */
  label: React.ReactNode;
  /** Help text shown below the label. */
  description?: React.ReactNode;
  /**
   * An error message to show. A non-empty message marks the group as
   * invalid. Without one, the group shows the browser's message after a
   * failed submit.
   */
  error?: React.ReactNode;
};

/**
 * A labeled group of `RadioGroupItem` options with one checked value, built
 * from UI SDK `Field` and `RadioGroup`. The field label names the group.
 * Arrow keys move between options.
 */
export function RadioGroup({
  label,
  description,
  error,
  className,
  disabled,
  ...props
}: RadioGroupProps) {
  const hasError =
    error !== undefined && error !== null && error !== false && error !== "";

  return (
    <Field
      className="dui-radio-field"
      disabled={disabled}
      invalid={hasError ? true : undefined}
    >
      <FieldLabel className="dui-radio-field-label">{label}</FieldLabel>
      {description === undefined ? null : (
        <FieldDescription className="dui-radio-field-description">
          {description}
        </FieldDescription>
      )}
      <RadioGroupPart
        className={mergeClassName("dui-radio-group", className)}
        disabled={disabled}
        {...props}
      />
      {hasError ? (
        <FieldError className="dui-radio-field-error" match>
          {error}
        </FieldError>
      ) : (
        <FieldError className="dui-radio-field-error" />
      )}
    </Field>
  );
}

export type RadioGroupItemProps = Omit<
  React.ComponentProps<typeof Radio>,
  "children"
> & {
  /** The visible name of the option. */
  children: React.ReactNode;
};

/** One option of a `RadioGroup`. Pressing its text checks it. */
export function RadioGroupItem({
  children,
  className,
  ...props
}: RadioGroupItemProps) {
  return (
    <label className="dui-radio-item">
      <Radio className={mergeClassName("dui-radio", className)} {...props}>
        <RadioIndicator className="dui-radio-indicator" />
      </Radio>
      <span className="dui-radio-item-label">{children}</span>
    </label>
  );
}
