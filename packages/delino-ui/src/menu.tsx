// SPDX-License-Identifier: Apache-2.0
import {
  Menu as MenuRoot,
  MenuContent as MenuContentPart,
  MenuGroup as MenuGroupPart,
  MenuGroupLabel as MenuGroupLabelPart,
  MenuItem as MenuItemPart,
  MenuSeparator as MenuSeparatorPart,
  MenuTrigger as MenuTriggerPart,
} from "@ui-sdk/react/menu";
import type * as React from "react";
import { ButtonSize, ButtonVariant } from "./button.tsx";
import { mergeClassName } from "./class-name.ts";

export { createMenuController } from "@ui-sdk/react/menu";
export type { MenuController } from "@ui-sdk/react/menu";

export type MenuProps = React.ComponentProps<typeof MenuRoot>;

/**
 * A list of commands that opens from a button, built from UI SDK `Menu`.
 * Give each `MenuItem` a `value` and handle `onAction`. A Promise returned
 * from `onAction` keeps the item pending and the menu open until it settles.
 */
export function Menu(props: MenuProps) {
  return <MenuRoot {...props} />;
}

export type MenuTriggerProps = React.ComponentProps<typeof MenuTriggerPart> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

/**
 * The button that opens the menu. It looks like a `Button` and defaults to
 * `Secondary`.
 */
export function MenuTrigger({
  variant = ButtonVariant.Secondary,
  size = ButtonSize.Medium,
  className,
  ...props
}: MenuTriggerProps) {
  return (
    <MenuTriggerPart
      data-variant={variant}
      data-size={size}
      className={mergeClassName("dui-button", className)}
      {...props}
    />
  );
}

export type MenuContentProps = React.ComponentProps<typeof MenuContentPart>;

/** The menu panel. It opens 8px from the trigger by default. */
export function MenuContent({
  className,
  sideOffset = 8,
  ...props
}: MenuContentProps) {
  return (
    <MenuContentPart
      className={mergeClassName("dui-menu", className)}
      sideOffset={sideOffset}
      {...props}
    />
  );
}

export type MenuItemProps = React.ComponentProps<typeof MenuItemPart>;

/**
 * One command. `destructive` shows it in the danger color, for commands such
 * as Delete.
 */
export function MenuItem({ className, ...props }: MenuItemProps) {
  return (
    <MenuItemPart
      className={mergeClassName("dui-menu-item", className)}
      {...props}
    />
  );
}

export type MenuGroupProps = React.ComponentProps<typeof MenuGroupPart>;

/** Related commands. Name it with a `MenuGroupLabel` inside. */
export function MenuGroup({ className, ...props }: MenuGroupProps) {
  return (
    <MenuGroupPart
      className={mergeClassName("dui-menu-group", className)}
      {...props}
    />
  );
}

export type MenuGroupLabelProps = React.ComponentProps<
  typeof MenuGroupLabelPart
>;

/** The visible name of a `MenuGroup`. */
export function MenuGroupLabel({ className, ...props }: MenuGroupLabelProps) {
  return (
    <MenuGroupLabelPart
      className={mergeClassName("dui-menu-group-label", className)}
      {...props}
    />
  );
}

export type MenuSeparatorProps = React.ComponentProps<typeof MenuSeparatorPart>;

/** A line between groups of commands. Focus skips it. */
export function MenuSeparator({ className, ...props }: MenuSeparatorProps) {
  return (
    <MenuSeparatorPart
      className={mergeClassName("dui-menu-separator", className)}
      {...props}
    />
  );
}
