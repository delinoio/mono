// SPDX-License-Identifier: Apache-2.0

export function renderFieldLabel(
  id: string | undefined,
  hasCustomControl: boolean,
) {
  // UI SDK predicts a generated label target before it binds the control and
  // overrides a direct htmlFor prop. A render element can supply the explicit
  // target for a default control. Custom controls can choose their own IDs.
  // Remove this workaround when UI SDK uses the control's ID during SSR.
  return id && !hasCustomControl ? <label htmlFor={id} /> : undefined;
}
