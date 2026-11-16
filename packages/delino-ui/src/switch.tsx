// SPDX-License-Identifier: Apache-2.0
import { Field, FieldDescription, FieldLabel } from "@ui-sdk/react/field";
import { Switch as SwitchPart, SwitchThumb } from "@ui-sdk/react/switch";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type SwitchProps = Omit<
  React.ComponentProps<typeof SwitchPart>,
  "children"
> & {
  /** The visible name of the setting. */
  label: React.ReactNode;
  /** More detail about the setting, shown below the label. */
  description?: React.ReactNode;
};

/**
 * A labeled on/off setting built from UI SDK `Field` and `Switch`. The field
 * connects the label and description to the switch.
 */
export function Switch({
  label,
  description,
  className,
  ...props
}: SwitchProps) {
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";

  return (
    <Field className="dui-switch-field">
      <div className="dui-switch-text">
        <FieldLabel className="dui-switch-label">{label}</FieldLabel>
        {/* Workaround: the UI SDK Field predicts the description id in
            aria-describedby before the field binds on the client, so static
            markup would point at a missing element. Always render the
            description and hide it when empty (display: none from the package
            styles). Remove when the UI SDK stops predicting an id for an
            unregistered description. */}
        <FieldDescription
          className="dui-switch-description"
          hidden={!hasDescription}
        >
          {hasDescription ? description : null}
        </FieldDescription>
      </div>
      <SwitchPart
        className={mergeClassName("dui-switch", className)}
        {...props}
      >
        <SwitchThumb className="dui-switch-thumb" />
      </SwitchPart>
    </Field>
  );
}
