// SPDX-License-Identifier: Apache-2.0
import {
  Tabs as TabsRoot,
  TabsList as TabsListPart,
  TabsPanel as TabsPanelPart,
  TabsTab as TabsTabPart,
} from "@ui-sdk/react/tabs";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

export type TabsProps = React.ComponentProps<typeof TabsRoot>;

/**
 * Sections of content with one shown at a time, built from UI SDK `Tabs`.
 * Give each `TabsTab` and its `TabsPanel` the same `value`.
 */
export function Tabs({ className, ...props }: TabsProps) {
  return (
    <TabsRoot className={mergeClassName("dui-tabs", className)} {...props} />
  );
}

export type TabsListProps = React.ComponentProps<typeof TabsListPart>;

/**
 * The row of tabs. Give it an accessible name with `aria-label`. Arrow keys
 * move between tabs.
 */
export function TabsList({ className, ...props }: TabsListProps) {
  return (
    <TabsListPart
      className={mergeClassName("dui-tabs-list", className)}
      {...props}
    />
  );
}

export type TabsTabProps = React.ComponentProps<typeof TabsTabPart>;

/** A tab that shows the panel with the same `value`. */
export function TabsTab({ className, ...props }: TabsTabProps) {
  return (
    <TabsTabPart
      className={mergeClassName("dui-tabs-tab", className)}
      {...props}
    />
  );
}

export type TabsPanelProps = React.ComponentProps<typeof TabsPanelPart>;

/** The content shown while the tab with the same `value` is selected. */
export function TabsPanel({ className, ...props }: TabsPanelProps) {
  return (
    <TabsPanelPart
      className={mergeClassName("dui-tabs-panel", className)}
      {...props}
    />
  );
}
