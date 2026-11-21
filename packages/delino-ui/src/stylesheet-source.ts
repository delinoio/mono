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
// text compares as a single line.
function compact(text: string): string {
  return text
    .replace(/\s+/g, " ")
    .replace(/\( /g, "(")
    .replace(/ \)/g, ")")
    .trim();
}

// Compares selector lists regardless of the whitespace around commas.
function normalizeSelector(selector: string): string {
  return compact(selector).replace(/ ?, ?/g, ", ");
}

/**
 * Returns the declarations of the first style rule in `source` whose complete
 * selector prelude is exactly `selector`, collapsed to one line, or `null`
 * when no rule matches.
 *
 * The source is scanned brace by brace instead of searched with a pattern, so
 * a query cannot match the tail of a longer descendant, compound or grouped
 * selector, and text inside comments is ignored. Rules nested in at-rules
 * (`@layer`, `@media`, `@keyframes`) are found; at-rule blocks themselves are
 * not style rules and never match. Rules are searched in source order.
 *
 * Limitation: braces or comment markers inside a CSS string or `url()` are not
 * recognized. `styles.css` has none; revisit this scanner if one is added.
 */
export function findRuleBlock(source: string, selector: string): string | null {
  const text = source.replace(/\/\*[\s\S]*?\*\//g, " ");
  const wanted = normalizeSelector(selector);
  const open: { prelude: string; bodyStart: number; hasChild: boolean }[] = [];
  let preludeStart = 0;
  for (let index = 0; index < text.length; index += 1) {
    const char = text[index];
    if (char === "{") {
      const parent = open[open.length - 1];
      if (parent) {
        parent.hasChild = true;
      }
      open.push({
        prelude: normalizeSelector(text.slice(preludeStart, index)),
        bodyStart: index + 1,
        hasChild: false,
      });
      preludeStart = index + 1;
    } else if (char === "}") {
      const closed = open.pop();
      if (
        closed &&
        !closed.hasChild &&
        !closed.prelude.startsWith("@") &&
        closed.prelude === wanted
      ) {
        return compact(text.slice(closed.bodyStart, index));
      }
      preludeStart = index + 1;
    } else if (char === ";") {
      preludeStart = index + 1;
    }
  }
  return null;
}

/**
 * Returns the declarations of the first rule of `styles.css` whose complete
 * selector is exactly `selector`, collapsed to one line.
 */
export function block(selector: string): string {
  const declarations = findRuleBlock(css, selector);
  expect(declarations).not.toBeNull();
  return declarations ?? "";
}
