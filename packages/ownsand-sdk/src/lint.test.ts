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
// Assembled from parts so that this file's own source does not look like an
// import from servers/ to the lint script.
const serversImport = ["import x from", '"../../servers/x"'].join(" ");

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
      "b.ts": `${header}\r\n${serversImport};\r\n`,
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
