// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";

// The runner starts commands without a shell, so it needs the CLI file behind
// pnpm's Windows command shim.
const manifestPath = createRequire(import.meta.url).resolve(
  "@rsbuild/core/package.json",
);
const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
  bin: { rsbuild: string };
};
export const rsbuildBin = resolve(dirname(manifestPath), manifest.bin.rsbuild);
