// SPDX-License-Identifier: Apache-2.0
import * as React from "react";

/**
 * `true` below a `Dialog`, `false` elsewhere. Kept in its own module so the
 * input components can read it without importing `dialog.tsx`.
 */
export const InDialogContext = React.createContext(false);

/**
 * Returns the `autoFocus` value to forward to the DOM input.
 *
 * Workaround: React `autoFocus` calls `focus()` while the dialog content
 * commits, before the UI SDK Dialog records the element that opened it as the
 * focus return target. The SDK then records the autofocused input itself and
 * cannot return focus to the opener when the dialog closes. Inside a `Dialog`
 * the `autoFocus` prop is therefore not forwarded; the input keeps
 * `data-autofocus`, which the SDK Dialog uses as its initial focus target, so
 * the input is still focused on open and focus returns to the opener on close.
 * Scope: inputs rendered below `Dialog` only (not `AlertDialog` or `Drawer`);
 * outside a dialog `autoFocus` is forwarded unchanged. Remove when the UI SDK
 * records the return target before React commits autofocus.
 */
export function useForwardedAutoFocus(
  autoFocus: boolean | undefined,
): boolean | undefined {
  const inDialog = React.useContext(InDialogContext);
  return inDialog ? undefined : autoFocus;
}
