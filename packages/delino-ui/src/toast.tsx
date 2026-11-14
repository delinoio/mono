// SPDX-License-Identifier: Apache-2.0
import {
  Toast,
  ToastAction,
  ToastClose,
  ToastDescription,
  ToastRegion as ToastRegionPart,
  ToastTitle,
} from "@ui-sdk/react/toast";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export { createToaster, toast } from "@ui-sdk/react/toast";
export type { ToastData, Toaster, ToastType } from "@ui-sdk/react/toast";

export type ToastRegionProps = Omit<
  React.ComponentProps<typeof ToastRegionPart>,
  "children"
>;

/**
 * Shows the toasts added with `toast()`, stacked at the bottom of the page
 * by default. Render it once near the app root. Each toast shows its title,
 * description, action and cancel button, and closes with Escape or a swipe.
 */
export function ToastRegion({
  className,
  side = "bottom",
  align = "center",
  ...props
}: ToastRegionProps) {
  return (
    <ToastRegionPart
      className={mergeClassName("dui-toast-region", className)}
      side={side}
      align={align}
      {...props}
    >
      {(item) => (
        <Toast className="dui-toast">
          <div className="dui-toast-text">
            {item.title == null ? null : (
              <ToastTitle className="dui-toast-title" />
            )}
            {item.description == null ? null : (
              <ToastDescription className="dui-toast-description" />
            )}
          </div>
          {item.action ? (
            <ToastAction
              className="dui-button dui-toast-button"
              data-variant="primary"
              data-size="small"
            />
          ) : null}
          {item.cancel ? (
            <ToastClose
              className="dui-button dui-toast-button"
              data-variant="secondary"
              data-size="small"
              action={item.cancel.onCancel}
            >
              {item.cancel.label}
            </ToastClose>
          ) : null}
        </Toast>
      )}
    </ToastRegionPart>
  );
}
