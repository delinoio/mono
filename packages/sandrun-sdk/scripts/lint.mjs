// SPDX-License-Identifier: Apache-2.0
// Source rules of the SandRun TypeScript SDK that the type checker does not
// cover (docs/packages-sandrun-sdk-foundation.md):
// - every hand-written source starts with the Apache-2.0 SPDX header;
// - the SDK does not log, so sources do not call `console`;
// - the SDK never imports code from `servers/`;
// - the SDK runs in browsers too, so runtime sources do not import `node:`
//   modules (tests and the test helpers under `testing/` may).
import { readdirSync, readFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

// URL.pathname is a URL path (percent-encoded, with a leading slash before a
// Windows drive letter), not a filesystem path, so convert the file URL.
const sourceRoot = fileURLToPath(new URL("../src/", import.meta.url));
const root = process.argv[2] ? resolve(process.argv[2]) : sourceRoot;
const spdxHeader = /^\/\/ SPDX-License-Identifier: Apache-2\.0\r?\n/;
const failures = [];

function walk(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(path);
    } else if (entry.name.endsWith(".ts")) {
      check(path);
    }
  }
}

function check(path) {
  const name = relative(root, path);
  const source = readFileSync(path, "utf8");
  if (!spdxHeader.test(source)) {
    failures.push(`${name}: missing the Apache-2.0 SPDX header`);
  }
  if (!name.endsWith(".test.ts") && /\bconsole\s*\./.test(source)) {
    failures.push(`${name}: the SDK must not log through console`);
  }
  const runtime = !name.endsWith(".test.ts") && !name.startsWith("testing");
  const specifiers = moduleSpecifiers(name, source);
  if (
    runtime &&
    specifiers.some((specifier) => specifier.startsWith("node:"))
  ) {
    failures.push(`${name}: runtime sources must not import node: modules`);
  }
  if (specifiers.some((specifier) => specifier.includes("servers/"))) {
    failures.push(`${name}: the SDK must not import from servers/`);
  }
}

function moduleSpecifiers(name, source) {
  const file = ts.createSourceFile(name, source, ts.ScriptTarget.Latest);
  const specifiers = [];

  function visit(node) {
    let specifier;
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) {
      specifier = node.moduleSpecifier;
    } else if (
      ts.isCallExpression(node) &&
      node.expression.kind === ts.SyntaxKind.ImportKeyword
    ) {
      specifier = node.arguments[0];
    }

    if (specifier && ts.isStringLiteral(specifier)) {
      specifiers.push(specifier.text);
    }
    ts.forEachChild(node, visit);
  }

  visit(file);
  return specifiers;
}

walk(root);
if (failures.length > 0) {
  for (const failure of failures) console.error(failure);
  process.exit(1);
}
console.log("sandrun-sdk: lint passed");
