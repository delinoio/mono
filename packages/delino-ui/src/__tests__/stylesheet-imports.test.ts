// SPDX-License-Identifier: Apache-2.0
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { expect, test } from "@rstest/core";
import { findUiSdkStylesheetImports } from "./stylesheet-imports.ts";

test("reports each UI SDK stylesheet import with its package-relative path", () => {
  const packageRoot = mkdtempSync(join(tmpdir(), "delino-ui-source-"));
  const violations = {
    "src/import.tsx": 'import\n"@ui-sdk/core/web.css";',
    "src/re-export.tsx": 'export * from\n"@ui-sdk/core/web.css";',
    "src/nested/component.tsx": 'import "@ui-sdk/core/web.css";',
    "src/nested/deeper/re-export.ts":
      "export { default as sheet }\nfrom '@ui-sdk/core/web.css';",
    "src/nested/style.css": '@import "@ui-sdk/core/web.css";',
    "src/nested/deeper/quoted.css":
      '@layer ui-sdk;\n@import /* theme */\nURL(\n"@ui-sdk/core/web.css"\n) layer(ui-sdk);',
    "src/nested/deeper/unquoted.css": "@import url(\n@ui-sdk/core/web.css\n);",
  };
  const allowed = {
    "src/components.tsx": [
      'import { Toast } from "@ui-sdk/react/toast";',
      'export { createToaster } from "@ui-sdk/react/toast";',
      "export { Toast };",
      '// import "@ui-sdk/core/web.css";',
      "const fixture = 'export * from \"@ui-sdk/core/web.css\";';",
      'const style = () => import("@ui-sdk/core/web.css");',
    ].join("\n"),
    "src/styles.css": [
      "@layer ui-sdk, delino-ui;",
      '/* @import "@ui-sdk/core/web.css"; */',
      ".toast::before { content: '@import \"@ui-sdk/core/web.css\";'; }",
      '@import "./other.css" layer(ui-sdk);',
    ].join("\n"),
    "src/nested/guard.test.ts": violations["src/import.tsx"],
    "src/nested/guard.test.tsx": violations["src/import.tsx"],
    "src/nested/guard.spec.ts": violations["src/import.tsx"],
    "src/nested/style.fixture.css": violations["src/nested/style.css"],
    "src/nested/__tests__/helper.ts": violations["src/import.tsx"],
    "src/nested/__fixtures__/style.css": violations["src/nested/style.css"],
    "src/nested/tests/fixtures/component.tsx": violations["src/import.tsx"],
  };
  try {
    for (const [path, source] of Object.entries({
      ...violations,
      ...allowed,
    })) {
      const filename = join(packageRoot, path);
      mkdirSync(dirname(filename), { recursive: true });
      writeFileSync(filename, source);
    }
    expect(findUiSdkStylesheetImports(packageRoot)).toEqual(
      Object.keys(violations).sort(),
    );
  } finally {
    rmSync(packageRoot, { recursive: true, force: true });
  }
});
