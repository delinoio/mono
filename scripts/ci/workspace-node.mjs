// SPDX-License-Identifier: Apache-2.0
import { realpathSync } from "node:fs";
import { pathToFileURL } from "node:url";

const minimumNodeVersion = "24.8.0";
const [minimumMajor, minimumMinor, minimumPatch] = minimumNodeVersion
  .split(".")
  .map(Number);

// Node.js before 24.2 did not expose import.meta.main. Identify the entry
// point so an older runtime can still report the version error through symlinks.
function isLegacyEntryPoint(url) {
  const entryPath = process.argv[1];
  if (entryPath === undefined) return false;
  if (pathToFileURL(entryPath).href === url) return true;
  try {
    return pathToFileURL(realpathSync(entryPath)).href === url;
  } catch {
    return false;
  }
}

export function isWorkspaceCli(meta) {
  if (!(meta.main ?? isLegacyEntryPoint(meta.url))) return false;

  const version = process.versions.node;
  const [major, minor, patch] = version.split(".").map(Number);
  const versionOrder =
    major - minimumMajor || minor - minimumMinor || patch - minimumPatch;
  if (versionOrder >= 0) return true;

  console.error(
    `Node.js ${version} is unsupported. This workspace requires Node.js ${minimumNodeVersion} or newer. ` +
      "Use the version pinned in .nvmrc. With nvm, run `nvm install` and `nvm use` from the repository root.",
  );
  process.exitCode = 1;
  return false;
}
