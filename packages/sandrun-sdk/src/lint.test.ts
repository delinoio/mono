// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "@rstest/core";

const script = fileURLToPath(new URL("../scripts/lint.mjs", import.meta.url));
const header = "// SPDX-License-Identifier: Apache-2.0\n";
const nodeSource =
  header +
  'import { readFileSync } from "node:fs";\nexport { readFileSync };\n';
const success = { status: 0, stdout: "sandrun-sdk: lint passed\n", stderr: "" };
const nodeDiagnostic = "runtime sources must not import node: modules";
const serverDiagnostic = "the SDK must not import from servers/";

function lint(source: string, name = "imports.ts") {
  const root = mkdtempSync(join(tmpdir(), "sandrun-sdk-lint-"));
  try {
    const path = join(root, name);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, source);
    const { status, stdout, stderr, error } = spawnSync(
      process.execPath,
      [script, root],
      {
        encoding: "utf8",
      },
    );
    if (error) throw error;
    return { status, stdout, stderr };
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

describe("sandrun-sdk lint module specifiers", () => {
  test.each([
    'import fs from "node:fs";',
    'import "node:fs";',
    'export { readFile } from "node:fs";',
    "void import('node:fs');",
    'void import("node:fs", { with: { type: "json" } });',
  ])("rejects runtime Node.js dependencies in %s", (statement) => {
    expect(lint(header + statement)).toEqual({
      status: 1,
      stdout: "",
      stderr: `imports.ts: ${nodeDiagnostic}\n`,
    });
  });

  test.each([
    'import fake from "../../../servers/fake";',
    "import '../../../servers/fake';",
    'export { fake } from "../../../servers/fake";',
    'void import("../../../servers/fake");',
    'void import("../../../servers/fake", { with: { type: "json" } });',
  ])("rejects server dependencies in %s", (statement) => {
    expect(lint(header + statement)).toEqual({
      status: 1,
      stdout: "",
      stderr: `imports.ts: ${serverDiagnostic}\n`,
    });
  });

  test.each([
    "adapter.ts",
    "testing-utils.ts",
    join("testing-utils", "adapter.ts"),
    "testing.ts",
    join("runtime", "testing", "adapter.ts"),
  ])("rejects Node.js dependencies in runtime source %s", (name) => {
    expect(lint(nodeSource, name)).toEqual({
      status: 1,
      stdout: "",
      stderr: `${name}: ${nodeDiagnostic}\n`,
    });
  });

  test.each([
    "adapter.test.ts",
    join("testing", "fake-helper.ts"),
    join("testing", "nested", "fake-helper.ts"),
  ])("allows Node.js dependencies in %s but rejects server imports", (name) => {
    expect(lint(nodeSource, name)).toEqual(success);
    expect(
      lint(header + 'void import("../../../servers/fake");', name),
    ).toEqual({
      status: 1,
      stdout: "",
      stderr: `${name}: ${serverDiagnostic}\n`,
    });
  });

  test("allows local imports, import-like text and computed specifiers", () => {
    const source = [
      'import "./local";',
      'void import("./local");',
      "// import { readFile } from 'node:fs';",
      "const example = 'import \"../../../servers/fake\";';",
      "const reExport = 'export * from \"../../../servers/fake\";';",
      'const moduleName = "node:fs";',
      "void import(moduleName);",
      "export { example };",
    ].join("\n");
    expect(lint(header + source)).toEqual(success);
  });

  test("still requires the SPDX header", () => {
    expect(lint('import "./local";')).toEqual({
      status: 1,
      stdout: "",
      stderr: "imports.ts: missing the Apache-2.0 SPDX header\n",
    });
  });

  test("still rejects console calls outside test files", () => {
    const source = header + 'console.log("message");';
    expect(lint(source)).toEqual({
      status: 1,
      stdout: "",
      stderr: "imports.ts: the SDK must not log through console\n",
    });
    expect(lint(source, "imports.test.ts")).toEqual(success);
  });
});
