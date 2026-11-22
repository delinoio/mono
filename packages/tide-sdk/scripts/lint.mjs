// SPDX-License-Identifier: Apache-2.0
// Source rules of the tide TypeScript SDK that the type checker does not
// cover (docs/packages-tide-sdk-foundation.md):
// - every hand-written source starts with the Apache-2.0 SPDX header;
// - the SDK does not log, so sources do not call `console`;
// - the SDK never imports code from `servers/`;
// - the SDK runs in browsers too, so runtime sources do not import `node:`
//   modules (tests and the test helpers under `testing/` may).
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

// URL.pathname is a URL path (percent-encoded, with a leading slash before a
// Windows drive letter), not a filesystem path, so convert the file URL.
const root = fileURLToPath(new URL("../src/", import.meta.url));
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
  if (!source.startsWith("// SPDX-License-Identifier: Apache-2.0\n")) {
    failures.push(`${name}: missing the Apache-2.0 SPDX header`);
  }
  if (!name.endsWith(".test.ts") && /\bconsole\s*\./.test(source)) {
    failures.push(`${name}: the SDK must not log through console`);
  }
  const runtime = !name.endsWith(".test.ts") && !name.startsWith("testing");
  if (runtime && /from\s+["']node:/.test(source)) {
    failures.push(`${name}: runtime sources must not import node: modules`);
  }
  if (/from\s+["'][^"']*servers\//.test(source)) {
    failures.push(`${name}: the SDK must not import from servers/`);
  }
}

walk(root);
if (failures.length > 0) {
  for (const failure of failures) console.error(failure);
  process.exit(1);
}
console.log("tide-sdk: lint passed");
