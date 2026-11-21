// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";

// These tests start the real launcher without a database. The test that runs
// the real server through the launcher needs Postgres and owns a generated
// database, so it is a Go test:
// TestLauncherForwardsSIGTERMToServerOnGeneratedDatabase in
// ../cmd/ownsand-api/launcher_signal_test.go.
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const Timeout = 120_000;

function start(env) {
  const child = spawn(process.execPath, [launcher], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
    shell: false,
  });
  let output = "";
  child.stdout.on("data", (d) => (output += d));
  child.stderr.on("data", (d) => (output += d));
  const exited = new Promise((resolve) =>
    child.once("close", (code, signal) => resolve({ code, signal })),
  );
  return { child, exited, output: () => output };
}

test("skips without DATABASE_URL", { timeout: Timeout }, async () => {
  const env = { ...process.env };
  delete env.DATABASE_URL;
  const run = start(env);
  const { code } = await run.exited;
  assert.equal(code, 0);
  assert.match(run.output(), /ownsand_api_dev_skipped/);
});
