// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { readWorkspacePackages } from "./workspace-packages.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const JOB = "deploy-static-sites";
const OVERHEAD_MINUTES = 15;

function assertPagesBudget(packages, waitSeconds, limitMinutes) {
  const count = packages.filter(
    ({ manifest }) => manifest.deploy?.target === "cloudflare-pages",
  ).length;
  assert.ok(count > 0, "no workspace package deploys to Cloudflare Pages");
  const needed = Math.ceil((count * waitSeconds) / 60) + OVERHEAD_MINUTES;
  assert.ok(
    limitMinutes >= needed,
    `${JOB} timeout-minutes is ${limitMinutes}, but ${count} Pages targets need at least ${needed}`,
  );
}

test("deploy-static-sites budgets a serial Pages wait per workspace target", () => {
  const workflow = readFileSync(
    join(root, ".github/workflows/deploy.yml"),
    "utf8",
  ).replaceAll("\r\n", "\n");
  const job = workflow.match(
    new RegExp(`^ {2}${JOB}:\\n((?:(?: {4}.*)?\\n)*)`, "m"),
  );
  assert.ok(job, `deploy.yml has no ${JOB} job`);
  const limit = Number(job[1].match(/^ {4}timeout-minutes: (\d+)$/m)?.[1]);
  assert.ok(Number.isInteger(limit), `${JOB} has no numeric timeout-minutes`);

  const provider = readFileSync(
    join(root, "scripts/ci/deploy-provider.sh"),
    "utf8",
  );
  const waitSeconds = Number(
    provider.match(
      /"\$\{DEPLOY_POLL_TIMEOUT:-(\d+)\}" read-pages pages_classify/,
    )?.[1],
  );
  assert.ok(waitSeconds > 0, "wait-pages has no positive default timeout");
  assertPagesBudget(readWorkspacePackages(root), waitSeconds, limit);
  assert.ok(limit < 360, `${JOB} timeout-minutes ${limit} reaches six hours`);
});

const pagesPackage = { manifest: { deploy: { target: "cloudflare-pages" } } };
const fiveSites = [
  ...Array.from({ length: 5 }, () => pagesPackage),
  { manifest: { deploy: { target: "droplet" } } },
  { manifest: {} },
];

test("five Pages targets need 65 minutes at the 600-second default", () => {
  assertPagesBudget(fiveSites, 600, 65);
  assert.throws(
    () => assertPagesBudget(fiveSites, 600, 30),
    /5 Pages targets need at least 65/,
  );
});

test("a sixth Pages target raises the minimum to 75 minutes", () => {
  const sixSites = [...fiveSites, pagesPackage];
  assert.throws(
    () => assertPagesBudget(sixSites, 600, 65),
    /6 Pages targets need at least 75/,
  );
  assertPagesBudget(sixSites, 600, 75);
});
