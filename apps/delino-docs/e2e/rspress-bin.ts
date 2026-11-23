// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const RSPRESS_PACKAGE = "@rspress/core";
const APP_DIRECTORY = fileURLToPath(new URL("..", import.meta.url));

// Returns the absolute path of the Rspress CLI file that the app resolves
// (`bin.rspress` of its `@rspress/core` package). Start it with the current
// Node executable (`process.execPath`) and an argument array: a bare `rspress`
// name is a `.cmd` shim on Windows, which neither Node's `spawn` without a
// shell nor the e2e runner (it runs `app.command` without a shell) can start.
// The file path also does not depend on `PATH`.
export const rspressBin = (): string => {
  const manifestPath = createRequire(
    resolve(APP_DIRECTORY, "package.json"),
  ).resolve(`${RSPRESS_PACKAGE}/package.json`);
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
    bin?: string | Record<string, string>;
  };
  const bin =
    typeof manifest.bin === "string" ? manifest.bin : manifest.bin?.rspress;
  if (bin === undefined) {
    throw new Error(`${RSPRESS_PACKAGE} declares no rspress bin`);
  }
  return resolve(dirname(manifestPath), bin);
};
