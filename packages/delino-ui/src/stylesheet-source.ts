// SPDX-License-Identifier: Apache-2.0
// Test-only helper (not exported from the package). happy-dom computes no
// layout and no cascade, so stylesheet-only behavior is checked against the
// stylesheet source.
import { readFileSync } from "node:fs";
import { expect } from "@rstest/core";

/** The raw text of `styles.css`. */
export const css = readFileSync(
  new URL("./styles.css", import.meta.url),
  "utf8",
);

// Collapse formatter line wraps (including wrapped selectors and values) so
// rules compare as single lines.
const compactCss = css
  .replace(/\s+/g, " ")
  .replace(/\( /g, "(")
  .replace(/ \)/g, ")");

/**
 * Returns the declarations of the first rule whose selector is exactly
 * `selector`, collapsed to one line. Every regex metacharacter in the selector
 * is escaped, so pseudo-classes such as `:dir(rtl)` and
 * `:hover:not([data-disabled])` match literally.
 */
export function block(selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = new RegExp(`${escaped} \\{([^}]*)\\}`).exec(compactCss);
  expect(match).not.toBeNull();
  return (match?.[1] ?? "").trim();
}
