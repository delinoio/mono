// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, test } from "@rstest/core";

const script = fileURLToPath(new URL("../scripts/lint.mjs", import.meta.url));
const header = "// SPDX-License-Identifier: Apache-2.0";
const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

// Lints a fixture source tree with the real lint script.
function lint(files: Record<string, string>) {
  const root = mkdtempSync(join(tmpdir(), "ownsand-sdk-lint-"));
  roots.push(root);
  for (const [name, content] of Object.entries(files)) {
    const path = join(root, name);
    mkdirSync(join(path, ".."), { recursive: true });
    // Write bytes as given: no newline translation happens.
    writeFileSync(path, content);
  }
  const result = spawnSync(process.execPath, [script, root], {
    encoding: "utf8",
  });
  return {
    status: result.status,
    stderr: result.stderr,
    stdout: result.stdout,
  };
}

describe("ownsand-sdk lint SPDX header", () => {
  test("accepts an LF-terminated header", () => {
    const result = lint({ "a.ts": `${header}\nexport const a = 1;\n` });
    expect(result.status).toBe(0);
  });

  test("accepts a CRLF-terminated header", () => {
    const result = lint({ "a.ts": `${header}\r\nexport const a = 1;\r\n` });
    expect(result.status).toBe(0);
  });

  test("rejects a missing header", () => {
    const result = lint({ "a.ts": "export const a = 1;\n" });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain("a.ts: missing the Apache-2.0 SPDX header");
  });

  test("rejects a different license identifier", () => {
    const result = lint({
      "a.ts": "// SPDX-License-Identifier: MIT\nexport const a = 1;\n",
    });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain("missing the Apache-2.0 SPDX header");
  });

  test("rejects a header with trailing text", () => {
    const result = lint({
      "a.ts": `${header} extra\r\nexport const a = 1;\r\n`,
    });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain("missing the Apache-2.0 SPDX header");
  });

  test("rejects a header without a line terminator", () => {
    const result = lint({ "a.ts": header });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain("missing the Apache-2.0 SPDX header");
  });

  test("still rejects console use and servers imports in CRLF sources", () => {
    const result = lint({
      "a.ts": `${header}\r\nconsole.log("x");\r\n`,
      "b.ts": `${header}\r\nimport x from "../../../servers/x";\r\n`,
    });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain(
      "a.ts: the SDK must not log through console",
    );
    expect(result.stderr).toContain(
      "b.ts: the SDK must not import from servers/",
    );
    expect(result.stderr).not.toContain("SPDX");
  });
});

describe("ownsand-sdk lint server imports", () => {
  test.each([
    'import x from "../../../servers/x";',
    'import "../../../servers/x";',
    'void import("../../../servers/x");',
    'export { x } from "../../../servers/x";',
    "void import(`../../../servers/x`);",
    'void import("../../../packages/../servers/x", {});',
    'export type Example = import("../../../servers/x").Example;',
    'export type Example = typeof import("../../../servers/x");',
  ])("rejects %s", (source) => {
    const result = lint({ "a.ts": `${header}\n${source}\n` });
    expect(result.status).toBe(1);
    expect(result.stderr).toBe("a.ts: the SDK must not import from servers/\n");
  });

  test.each([
    'import "../../../../servers/x";',
    'export type Example = import("../../../../servers/x").Example;',
  ])("rejects %s in a nested source file", (source) => {
    const result = lint({
      "nested/a.ts": `${header}\n${source}\n`,
    });
    expect(result.status).toBe(1);
    expect(result.stderr).toBe(
      `${join("nested", "a.ts")}: the SDK must not import from servers/\n`,
    );
  });

  test.each([
    'import x from "./local";',
    'import "./local";',
    'void import("./local");',
    'export type Example = import("./local").Example;',
    'import x from "./servers/local";',
    '// import x from "../../../servers/x";',
    '/* import "../../../servers/x"; void import("../../../servers/x"); */',
    "const example = 'import x from \"../../../servers/x\";';",
    'const example = `void import("../../../servers/x");`;',
    "void import(`../../../servers/${name}`);",
  ])("allows %s", (source) => {
    const result = lint({ "a.ts": `${header}\n${source}\n` });
    expect(result.status).toBe(0);
    expect(result.stderr).toBe("");
  });
});
