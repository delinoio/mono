// SPDX-License-Identifier: Apache-2.0
// happy-dom computes no layout and no cascade, so these tests check the
// stylesheet source. The rendered widths are checked in a real browser by the
// OwnSand console end-to-end test for a long dialog title.
import { describe, expect, test } from "@rstest/core";
import { block } from "./stylesheet-source.ts";

describe("long text in dialogs and the select popup", () => {
  // Without it, a name with no space (for example an organization or template
  // name) keeps the title on one line, and the line widens the dialog panel
  // and scrolls it sideways. Titles with spaces still break at the spaces
  // first, and a short title stays on one line.
  test("lets a dialog title break an unbroken word inside the panel", () => {
    const title = block(".dui-dialog-title");
    expect(title).toContain("overflow-wrap: anywhere");
    expect(title).not.toContain("white-space: nowrap");
    expect(title).not.toContain("text-overflow");
  });

  test("keeps the select popup inside the viewport", () => {
    const content = block(".dui-select-content");
    // The popup stays at least as wide as the trigger.
    expect(content).toContain("min-width: var(--anchor-width)");
    // `max-width` wins over `min-width`, so a wide trigger cannot push the
    // popup past the viewport either. 16px is 8px on each side.
    expect(content).toContain("max-width: calc(100vw - 16px)");
  });

  test("lets an option break an unbroken word instead of widening the popup", () => {
    expect(block(".dui-select-option")).toContain("overflow-wrap: anywhere");
  });

  test("keeps the select trigger value truncated with an ellipsis", () => {
    const value = block(".dui-select-value");
    expect(value).toContain("text-overflow: ellipsis");
    expect(value).toContain("white-space: nowrap");
    expect(value).not.toContain("overflow-wrap");
  });
});
