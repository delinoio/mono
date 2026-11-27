// SPDX-License-Identifier: Apache-2.0
import * as React from "react";

export enum PanelAutoFocus {
  Default,
  Deferred,
  DialogOpening,
  DialogOpen,
}

export const PanelAutoFocusContext = React.createContext(
  PanelAutoFocus.Default,
);

/**
 * The SDK selects initial focus by querying for `[autofocus], [data-autofocus]`.
 * React normally calls focus() at mount without emitting that attribute. During
 * opening, the marker lets the SDK select the field after capturing the opener.
 * Remove this workaround when the SDK can use React autofocus and still capture
 * the opener and honor its initial focus policy.
 */
export function useFieldAutoFocus(
  autoFocus: boolean | undefined,
  disabled: boolean | undefined,
) {
  const mode = React.useContext(PanelAutoFocusContext);
  const inDialog =
    mode === PanelAutoFocus.DialogOpening || mode === PanelAutoFocus.DialogOpen;

  return {
    autoFocus:
      mode === PanelAutoFocus.Default ||
      (mode === PanelAutoFocus.DialogOpen && !disabled)
        ? autoFocus
        : undefined,
    // The SDK only checks the first marker, so a disabled Dialog control can
    // hide the next eligible target. Callers can still supply their own marker.
    marker: autoFocus && !(inDialog && disabled) ? "" : undefined,
  };
}
