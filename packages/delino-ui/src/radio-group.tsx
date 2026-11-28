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
import * as React from "react";
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
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";

  return (
    <Field
      className="dui-radio-field"
      disabled={disabled}
      invalid={hasError && !disabled ? true : undefined}
    >
      <FieldLabel className="dui-radio-field-label">{label}</FieldLabel>
      {/* Workaround: the UI SDK Field predicts the description id in
          aria-describedby before the field binds on the client, so static
          markup would point at a missing element. Always render the
          description and hide it when empty (display: none from the package
          styles). Remove when the UI SDK stops predicting an id for an
          unregistered description. */}
      <FieldDescription
        className="dui-radio-field-description"
        hidden={!hasDescription}
      >
        {hasDescription ? description : null}
      </FieldDescription>
      <RadioGroupPart
        className={mergeClassName("dui-radio-group", className)}
        disabled={disabled}
        {...props}
      />
      {hasError ? (
        <FieldError className="dui-radio-field-error">{error}</FieldError>
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

function activateCustomRadio(event: React.MouseEvent<HTMLLabelElement>) {
  const label = event.currentTarget;
  const target = event.target;
  if (
    event.defaultPrevented ||
    !(target instanceof Element) ||
    !label.control?.matches('[data-slot="radio-input"]')
  ) {
    return;
  }

  const radio = label.querySelector<HTMLElement>('[data-slot="radio"]');
  const interactive = target.closest(
    "a[href], audio[controls], button, details, embed, iframe, img[usemap], input, label, select, textarea, video[controls]",
  );
  if (!radio || radio.contains(target) || interactive !== label) {
    return;
  }

  // The SDK cancels clicks forwarded to the hidden input for non-labelable
  // radios. That makes the browser restore the previous checked input after the
  // SDK has selected a new value. Forward the label click to the visible radio
  // instead. Remove this when the SDK keeps the input in sync after label clicks.
  event.preventDefault();
  if (radio.getAttribute("aria-disabled") !== "true") {
    radio.focus();
    radio.click();
  }
}

function withRadioLabel(
  radio: React.ReactElement<React.AriaAttributes>,
  labelId: string,
  itemProps?: React.AriaAttributes,
) {
  const {
    "aria-label": ariaLabel = itemProps?.["aria-label"],
    "aria-labelledby": ariaLabelledBy = itemProps?.["aria-labelledby"],
  } = radio.props;
  return ariaLabel?.trim() || ariaLabelledBy?.trim()
    ? radio
    : React.cloneElement(radio, { "aria-labelledby": labelId });
}

/** One option of a `RadioGroup`. Pressing its text checks it. */
export function RadioGroupItem({
  children,
  className,
  render,
  ...props
}: RadioGroupItemProps) {
  const labelId = React.useId();
  // The SDK names a non-labelable custom radio from its hidden input after
  // mount. Supply the visible label during render until the SDK can do so
  // before hydration. Check the composed element for any caller name first.
  const renderRadio: RadioGroupItemProps["render"] =
    typeof render === "function"
      ? (radioProps, state) => {
          const radio = render(radioProps, state);
          return React.isValidElement<React.AriaAttributes>(radio)
            ? withRadioLabel(radio, labelId)
            : radio;
        }
      : render && withRadioLabel(render, labelId, props);

  return (
    <label
      className="dui-radio-item"
      onClick={render ? activateCustomRadio : undefined}
    >
      <Radio
        className={mergeClassName("dui-radio", className)}
        render={renderRadio}
        {...props}
      >
        <RadioIndicator className="dui-radio-indicator" />
      </Radio>
      <span className="dui-radio-item-label" id={render ? labelId : undefined}>
        {children}
      </span>
    </label>
  );
}
