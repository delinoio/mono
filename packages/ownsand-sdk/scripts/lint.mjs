// SPDX-License-Identifier: Apache-2.0
// Source rules of the OwnSand TypeScript SDK that the type checker does not
// cover (docs/packages-ownsand-sdk-foundation.md):
// - every hand-written source starts with the Apache-2.0 SPDX header;
// - the SDK logs nothing by default, so sources do not call `console`;
// - the SDK never imports code from `servers/`.
import { readdirSync, readFileSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

// URL.pathname is a URL path (percent-encoded, with a leading slash before a
// Windows drive letter), not a filesystem path, so convert the file URL.
// An optional first argument replaces the default source root so that tests
// can lint fixture trees.
const sourceRoot = fileURLToPath(new URL("../src/", import.meta.url));
const root = process.argv[2] ? resolve(process.argv[2]) : sourceRoot;
const serversRoot = fileURLToPath(
  new URL("../../../servers/", import.meta.url),
);
// The header line ends in LF or CRLF: a checkout with `core.autocrlf=true`
// converts every source to CRLF.
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
  if (importsFromServers(name, source)) {
    failures.push(`${name}: the SDK must not import from servers/`);
  }
}

function importsFromServers(name, source) {
  const file = ts.createSourceFile(name, source, ts.ScriptTarget.Latest);
  // Resolve paths as if the fixture tree were under the SDK source root.
  const sourceDir = dirname(resolve(sourceRoot, name));

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

    if (specifier && ts.isStringLiteralLike(specifier)) {
      const path = specifier.text;
      if (path.startsWith("./") || path.startsWith("../") || isAbsolute(path)) {
        const target = relative(serversRoot, resolve(sourceDir, path));
        if (
          target !== ".." &&
          !target.startsWith(`..${sep}`) &&
          !isAbsolute(target)
        ) {
          return true;
        }
      }
    }

    return ts.forEachChild(node, visit);
  }

  return visit(file);
}

walk(root);
if (failures.length > 0) {
  for (const failure of failures) console.error(failure);
  process.exit(1);
}
console.log("ownsand-sdk: lint passed");
