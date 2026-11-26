// SPDX-License-Identifier: Apache-2.0
//
// Budget of the `sign-macos-binaries` job of `.github/workflows/deploy.yml`:
// the job notarizes every darwin executable one after another, so its
// `timeout-minutes` must cover one full notarization wait per darwin asset.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { readWorkspacePackages } from "./workspace-packages.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
// Minutes for the setup, the signing and the checks around the waits.
const OVERHEAD_MINUTES = 15;
const JOB = "sign-macos-binaries";

function read(path) {
  return readFileSync(join(root, path), "utf8");
}

// The number of darwin assets that `release-binaries.sh sign-macos` handles.
function darwinAssetCount() {
  let count = 0;
  for (const { manifest } of readWorkspacePackages(root)) {
    if (manifest.deploy?.target !== "github-release") continue;
    count += manifest.deploy.platforms.filter((p) =>
      p.startsWith("darwin-"),
    ).length;
  }
  return count;
}

function jobBlock() {
  const workflow = read(".github/workflows/deploy.yml").replaceAll(
    "\r\n",
    "\n",
  );
  const match = workflow.match(
    new RegExp(`^ {2}${JOB}:\\n((?:(?: {4}.*)?\\n)*)`, "m"),
  );
  assert.ok(match, `deploy.yml has no ${JOB} job`);
  return match[1];
}

test("the notarization wait per asset defaults to 45 minutes", () => {
  assert.match(
    read("scripts/ci/release-binaries.sh"),
    /--timeout "\$\{NOTARY_TIMEOUT:-45m\}"/,
  );
});

test("sign-macos-binaries budgets a serial notarization wait per darwin asset", () => {
  const assets = darwinAssetCount();
  assert.ok(assets > 0, "no release binary has a darwin platform");
  const limit = Number(jobBlock().match(/^ {4}timeout-minutes: (\d+)$/m)?.[1]);
  assert.ok(Number.isInteger(limit), `${JOB} has no timeout-minutes`);
  const needed = assets * 45 + OVERHEAD_MINUTES;
  assert.ok(
    limit >= needed,
    `${JOB} timeout-minutes is ${limit}, but ${assets} darwin assets need at least ${needed}`,
  );
  // The limit stays below the six-hour GitHub default.
  assert.ok(limit < 360, `${JOB} timeout-minutes ${limit} reaches six hours`);
});

test("the workflow contract lists the sign-macos-binaries limit", () => {
  const limit = jobBlock().match(/^ {4}timeout-minutes: (\d+)$/m)?.[1];
  const contract = read("docs/repository-workflow-contract.md");
  assert.ok(
    contract.includes(`\`${JOB}\` ${limit},`),
    `the contract does not list ${JOB} ${limit}`,
  );
});
