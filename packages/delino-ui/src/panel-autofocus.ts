// SPDX-License-Identifier: Apache-2.0
import * as React from "react";

export const DeferAutoFocusContext = React.createContext(false);

/**
 * Returns the `autoFocus` value to forward to the DOM input.
 *
 * React focuses an input with `autoFocus` before the UI SDK captures the
 * opener for focus return. Inside Dialog, AlertDialog and Drawer panels,
 * defer autofocus to the SDK, which reads the input's `data-autofocus` marker
 * after it captures the opener. Fields outside the panels can focus on mount.
 * Remove this workaround when the SDK captures the opener before React
 * commits autofocus and preserves its initial focus policy.
 */
export function useForwardedAutoFocus(
  autoFocus: boolean | undefined,
): boolean | undefined {
  const deferAutoFocus = React.useContext(DeferAutoFocusContext);
  return deferAutoFocus ? undefined : autoFocus;
}
