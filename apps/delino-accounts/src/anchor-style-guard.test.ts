// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { describe, expect, test } from "@rstest/core";

// Guards the global anchor rules in index.css. They are unlayered, so they
// beat the layered `delino-ui` button colors. An anchor that renders a
// `delino-ui` Button (the toast action "Open billing") must stay outside
// them, or its text takes the link color and disappears on the accent
// background. The exclusion sits in `:where()` so that the rules keep the
// specificity of the plain `a` rules. With `a:not(.dui-button)` they would
// have one class more, beat one-class rules such as `.list-link` and
// `.button-link`, and recolor those links.
const css = readFileSync(new URL("./index.css", import.meta.url), "utf8");

/** The selectors of every top-level rule whose selector starts with `a`. */
function anchorSelectors(source: string): string[] {
  return Array.from(source.matchAll(/^(a[^{\n]*)\{/gm), (m) =>
    (m[1] ?? "").trim(),
  );
}

describe("global anchor rules", () => {
  test("exclude anchors that render a delino-ui Button", () => {
    expect(anchorSelectors(css)).toEqual([
      "a:where(:not(.dui-button))",
      "a:where(:not(.dui-button)):hover",
      "a:where(:not(.dui-button)):focus-visible",
    ]);
  });
});
