// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import test from "node:test";

const root = new URL("../../", import.meta.url);
const entryPoints = [
  "scripts/ci/comparison.mjs",
  "scripts/ci/deploy-plan.mjs",
  "scripts/ci/format-changed.mjs",
  "scripts/ci/plan.mjs",
  "scripts/dev/rspress-preview.mjs",
  "scripts/dev/start-portless-proxy.mjs",
  "scripts/dev/with-infisical.mjs",
  "servers/accounts-api/scripts/dev.mjs",
  "servers/ownsand-api/scripts/dev.mjs",
  "servers/sandrun-api/scripts/dev.mjs",
];

function runCli(args) {
  const result = spawnSync(process.execPath, args, {
    cwd: root,
    env: {
      ...process.env,
      DATABASE_URL: "",
      GITHUB_OUTPUT: "",
      GITHUB_STEP_SUMMARY: "",
      PORTLESS: "0",
    },
    encoding: "utf8",
    timeout: 10_000,
  });
  assert.ifError(result.error);
  return result;
}

function runWithVersion(version, args) {
  // Simulate the detected version with the installed Node.js binary so the
  // tooling suite does not need to install extra runtimes.
  const preload = `Object.defineProperty(process.versions, "node", { value: ${JSON.stringify(version)} });`;
  return runCli([
    "--import",
    `data:text/javascript,${encodeURIComponent(preload)}`,
    ...args,
  ]);
}

test("workspace CLI entry points reject unsupported Node.js before work", () => {
  for (const entry of entryPoints) {
    const version = "24.7.0";
    const result = runWithVersion(version, [entry]);
    assert.equal(result.status, 1, entry);
    assert.equal(result.stdout, "", entry);
    assert.match(result.stderr, /^Node\.js 24\.7\.0 is unsupported\./, entry);
    assert.match(result.stderr, /requires Node\.js 24\.8\.0 or newer/, entry);
    assert.match(result.stderr, /\.nvmrc.*nvm install.*nvm use/, entry);
  }
});

test("the CLI guard detects a direct invocation without import.meta.main", () => {
  const script = "scripts/ci/format-changed.mjs";
  const helper = new URL("scripts/ci/workspace-node.mjs", root).href;
  const url = new URL(script, root).href;
  const source = `import { isWorkspaceCli } from ${JSON.stringify(helper)};
isWorkspaceCli({ url: ${JSON.stringify(url)} });`;
  const result = runWithVersion("24.0.0", [
    "--input-type=module",
    "--eval",
    source,
    script,
  ]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "");
  assert.match(result.stderr, /^Node\.js 24\.0\.0 is unsupported\./);
});

test("the formatter enters its CLI at the workspace minimum and pinned versions", () => {
  const manifest = JSON.parse(
    readFileSync(new URL("package.json", root), "utf8"),
  );
  const pin = readFileSync(new URL(".nvmrc", root), "utf8").trim();
  assert.equal(manifest.engines.node, ">=24.8.0");
  assert.match(pin, /^24\.\d+\.\d+$/);

  for (const version of ["24.8.0", pin]) {
    const result = runWithVersion(version, [
      "scripts/ci/format-changed.mjs",
      "--not-a-valid-option",
    ]);
    assert.equal(result.status, 2, version);
    assert.match(
      result.stderr,
      /ci_format_error.*--not-a-valid-option/,
      version,
    );
  }
});

test("importing the workspace CLIs does not start their commands", () => {
  const source = entryPoints
    .map((entry) => `import ${JSON.stringify(new URL(entry, root).href)};`)
    .join("\n");
  const result = runCli(["--input-type=module", "--eval", source]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout, "");
  assert.equal(result.stderr, "");
});
