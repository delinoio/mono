// SPDX-License-Identifier: Apache-2.0
import type { ChangeDetails } from "@ui-sdk/core";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import {
  Select as SelectRoot,
  SelectContent,
  SelectItem,
  SelectItemIndicator,
  SelectItemText,
  type SelectProps as SelectRootProps,
  SelectTrigger,
  SelectValue,
  type SelectValueChangeReason,
} from "@ui-sdk/react/select";
import * as React from "react";
import { mergeClassName } from "./class-name.ts";

type SingleSelectProps = SelectRootProps<string, false>;

export type SelectProps = Omit<
  SingleSelectProps,
  "children" | "multiple" | "value" | "defaultValue" | "onValueChange"
> & {
  /** The selected option's value. Pass it to control the select. */
  value?: string | null;
  /** The option selected at first, when uncontrolled. */
  defaultValue?: string | null;
  /** Runs when the user or a form reset wants to change the value. */
  onValueChange?: (
    value: string | null,
    details: ChangeDetails<SelectValueChangeReason>,
  ) => void;
  /** The visible name of the field. */
  label: React.ReactNode;
  /** Help text shown below the trigger. */
  description?: React.ReactNode;
  /**
   * An error message to show. A non-empty message marks the field as
   * invalid. Without one, the field shows the browser's message after a
   * failed submit.
   */
  error?: React.ReactNode;
  /** `SelectOption` elements. */
  children: React.ReactNode;
  /** Classes for the trigger button. */
  className?: string;
  /** Disables the select. */
  disabled?: boolean;
};

/**
 * A labeled single-value picker built from UI SDK `Field` and `Select`. The
 * trigger shows the selected option, or `placeholder`, and opens a list of
 * `SelectOption` elements.
 */
export function Select({
  label,
  description,
  error,
  className,
  disabled,
  children,
  ...props
}: SelectProps) {
  // The type already omits `multiple`, but types do not stop JS callers or
  // untyped spread objects. Drop the key at runtime, silently, so `Select`
  // always stays a single-value picker. Remove this only if the type stops
  // omitting `multiple`.
  const { multiple: _multiple, ...rootProps } = props as typeof props & {
    multiple?: unknown;
  };
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";
  const hasError =
    error !== undefined && error !== null && error !== false && error !== "";

  return (
    <Field
      className="dui-text-field"
      disabled={disabled}
      invalid={hasError ? true : undefined}
    >
      <FieldLabel className="dui-text-field-label">{label}</FieldLabel>
      <SelectRoot disabled={disabled} {...rootProps}>
        <SelectTrigger
          className={className ? `dui-select ${className}` : "dui-select"}
        >
          <SelectValue className="dui-select-value" />
          <svg
            aria-hidden="true"
            className="dui-select-icon"
            viewBox="0 0 16 16"
            fill="none"
          >
            <path
              d="M4 6l4 4 4-4"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </SelectTrigger>
        <SelectContent className="dui-select-content">
          {withBigintOptionLabels(children)}
        </SelectContent>
      </SelectRoot>
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

export type SelectOptionProps = Omit<
  React.ComponentProps<typeof SelectItem>,
  "children"
> & {
  /**
   * The visible name of the option. Strings, numbers, bigints and nested
   * elements are allowed. An option whose children hold no text shows its value.
   */
  children: React.ReactNode;
};

/**
 * Collects text from strings, numbers and bigints. Reads arrays and element
 * children in order and skips booleans, null and undefined.
 */
function nodeText(node: React.ReactNode): string {
  if (
    typeof node === "string" ||
    typeof node === "number" ||
    typeof node === "bigint"
  ) {
    return String(node);
  }
  if (Array.isArray(node)) {
    return node.map(nodeText).join("");
  }
  if (React.isValidElement<{ children?: React.ReactNode }>(node)) {
    return nodeText(node.props.children);
  }
  return "";
}

function hasBigintChild(node: React.ReactNode): boolean {
  if (Array.isArray(node)) {
    return node.some(hasBigintChild);
  }
  if (React.isValidElement<{ children?: React.ReactNode }>(node)) {
    return hasBigintChild(node.props.children);
  }
  return typeof node === "bigint";
}

// UI SDK learns selected labels from the JSX tree before SelectOption renders.
// Supply the derived label for options with bigint children so closed and
// server-rendered selects show those digits. Remove this step when UI SDK
// learns bigint text from children itself.
function withBigintOptionLabels(node: React.ReactNode): React.ReactNode {
  if (Array.isArray(node)) {
    return node.map(withBigintOptionLabels);
  }
  if (
    !React.isValidElement<{ children?: React.ReactNode; label?: string }>(node)
  ) {
    return node;
  }

  const { children, label } = node.props;
  if (node.type === SelectOption) {
    return label == null && hasBigintChild(children)
      ? React.cloneElement(node, { label: nodeText(children) })
      : node;
  }

  const nested = withBigintOptionLabels(children);
  return nested === children
    ? node
    : React.cloneElement(node, { children: nested });
}

/** One option of a `Select`. A check marks the selected option. */
export function SelectOption({
  children,
  className,
  label,
  ...props
}: SelectOptionProps) {
  // The UI SDK keeps the trigger and the list text of an option from its
  // `label` (a string) and falls back to the option value when it is absent.
  // Interpolated children such as `{os} base image` reach this component as
  // an array, so derive the text here; an explicit `label` wins and an option
  // without text keeps showing its value.
  const text = nodeText(children);
  return (
    <SelectItem
      className={mergeClassName("dui-select-option", className)}
      label={label ?? (text === "" ? undefined : text)}
      {...props}
    >
      <SelectItemText>{children}</SelectItemText>
      <SelectItemIndicator className="dui-select-option-indicator">
        <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
          <path
            d="M3.5 8.5l3 3 6-7"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </SelectItemIndicator>
    </SelectItem>
  );
}
