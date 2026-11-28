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

type OpenBlock = { prelude: string; bodyStart: number; hasChild: boolean };

// This scanner treats braces and comment markers as syntax even inside CSS
// strings and url(). styles.css does not use those characters in either place.
function findBlock(
  source: string,
  matches: (block: OpenBlock) => boolean,
): string | null {
  const text = source.replace(/\/\*[\s\S]*?\*\//g, " ");
  const open: OpenBlock[] = [];
  let preludeStart = 0;
  for (let index = 0; index < text.length; index += 1) {
    const char = text[index];
    if (char === "{") {
      const parent = open[open.length - 1];
      if (parent) {
        parent.hasChild = true;
      }
      open.push({
        prelude: text.slice(preludeStart, index).trim(),
        bodyStart: index + 1,
        hasChild: false,
      });
      preludeStart = index + 1;
    } else if (char === "}") {
      const closed = open.pop();
      if (closed && matches(closed)) {
        return text.slice(closed.bodyStart, index);
      }
      preludeStart = index + 1;
    } else if (char === ";") {
      preludeStart = index + 1;
    }
  }
  return null;
}

/**
 * Returns the declarations of the first style rule with the complete selector.
 * This includes style rules nested in at-rules. The declarations are collapsed
 * to one line. Returns `null` when no rule matches.
 */
export function findRuleBlock(source: string, selector: string): string | null {
  const wanted = normalizeSelector(selector);
  const body = findBlock(
    source,
    ({ prelude, hasChild }) =>
      !hasChild &&
      !prelude.startsWith("@") &&
      normalizeSelector(prelude) === wanted,
  );
  return body === null ? null : compact(body);
}

/**
 * Returns the comment-free body of a complete at-rule with the given prelude,
 * including its nested rules. Returns `null` when no complete block matches.
 */
export function findAtRuleBlock(source: string, atRule: string): string | null {
  const wanted = compact(atRule);
  return findBlock(
    source,
    ({ prelude }) => prelude.startsWith("@") && compact(prelude) === wanted,
  );
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
