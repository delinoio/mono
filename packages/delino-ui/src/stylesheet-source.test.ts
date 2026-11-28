// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";

import { block, findAtRuleBlock, findRuleBlock } from "./stylesheet-source.ts";

describe("stylesheet source helper", () => {
  test("finds a rule by its complete selector", () => {
    const source = ".a { color: red; }\n.b {\n  color: blue;\n}";
    expect(findRuleBlock(source, ".a")).toBe("color: red;");
    expect(findRuleBlock(source, ".b")).toBe("color: blue;");
  });

  test("does not match the suffix of a descendant selector", () => {
    const source = ".parent .child { color: red; }";
    expect(findRuleBlock(source, ".child")).toBeNull();
    expect(findRuleBlock(source, ".parent .child")).toBe("color: red;");
  });

  test("does not match the suffix of a compound selector", () => {
    const source = ".a.b { color: red; }\n.a > .b { color: blue; }";
    expect(findRuleBlock(source, ".b")).toBeNull();
    expect(findRuleBlock(source, ".a.b")).toBe("color: red;");
    expect(findRuleBlock(source, ".a > .b")).toBe("color: blue;");
  });

  test("does not match one member of a grouped selector", () => {
    const source = ".a, .b { color: red; }\n.b { color: blue; }";
    expect(findRuleBlock(source, ".b")).toBe("color: blue;");
    expect(findRuleBlock(".a, .b { color: red; }", ".b")).toBeNull();
    expect(findRuleBlock(".a,\n.b { color: red; }", ".a, .b")).toBe(
      "color: red;",
    );
  });

  test("ignores rules and selectors inside comments", () => {
    const source =
      "/* .ghost { color: red; } */\n.real { color: blue; }\n/* .real { color: green; } */";
    expect(findRuleBlock(source, ".ghost")).toBeNull();
    expect(findRuleBlock(source, ".real")).toBe("color: blue;");
    expect(findRuleBlock("/* .x */ .y { top: 0; }", ".x")).toBeNull();
  });

  test("finds rules nested in at-rules and skips the at-rule blocks", () => {
    const source = [
      "@layer ui {",
      "  :root { color: black; }",
      "  @media (prefers-color-scheme: dark) {",
      "    :root { color: white; }",
      "    .a:is(.x, .y){ color: gray; }",
      "  }",
      "  @keyframes spin { from { opacity: 0; } to { opacity: 1; } }",
      "}",
    ].join("\n");
    expect(findRuleBlock(source, ":root")).toBe("color: black;");
    expect(findRuleBlock(source, ".a:is(.x, .y)")).toBe("color: gray;");
    expect(findRuleBlock(source, "from")).toBe("opacity: 0;");
    expect(findRuleBlock(source, "@media (prefers-color-scheme: dark)")).toBe(
      null,
    );
    expect(findRuleBlock(source, "@layer ui")).toBeNull();
  });

  test("returns the first rule in source order", () => {
    const source =
      "@media (min-width: 1px) { .a { top: 1px; } }\n.a { top: 2px; }";
    expect(findRuleBlock(source, ".a")).toBe("top: 1px;");
  });

  test("limits rule lookup to the complete matching at-rule body", () => {
    const media = "@media (forced-colors: active)";
    const source = [
      "@layer delino-ui {",
      `  /* ${media} { .ghost { color: red; } } */`,
      `  ${media} and (min-width: 1px) { .inside { color: red; } }`,
      "  @media (forced-colors: active) {",
      "    .first { color: Canvas; }",
      "    /* Ignore these braces: } { */",
      "    @supports (color: Highlight) {",
      "      .inside { color: Highlight; }",
      "    }",
      "  }",
      "  .outside { color: Highlight; }",
      "}",
    ].join("\n");

    const body = findAtRuleBlock(source, media);
    expect(body).not.toBeNull();
    expect(findRuleBlock(body ?? "", ".first")).toBe("color: Canvas;");
    expect(findRuleBlock(body ?? "", ".inside")).toBe("color: Highlight;");
    expect(findRuleBlock(body ?? "", ".outside")).toBeNull();
  });

  test("returns null for a missing or unclosed at-rule block", () => {
    const media = "@media (forced-colors: active)";
    expect(findAtRuleBlock(`/* ${media} {} */ .outside {}`, media)).toBeNull();
    expect(findAtRuleBlock(`${media} { .inside {}`, media)).toBeNull();
  });

  test("block() reads the packaged stylesheet by complete selector", () => {
    expect(block(".dui-button")).toContain("display:");
    expect(() => block(".dui-selector-that-does-not-exist")).toThrow();
  });
});
