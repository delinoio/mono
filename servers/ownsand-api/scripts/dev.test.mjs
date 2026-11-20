// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { fileURLToPath } from "node:url";
import test from "node:test";

// These tests start the real launcher. The signal test also builds and runs
// the real server, so it needs OWNSAND_TEST_DATABASE_URL (the server applies
// its migrations to that database); it is skipped without it and on Windows.
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const databaseURL = process.env.OWNSAND_TEST_DATABASE_URL;
const Timeout = 120_000;
const StatusSigterm = 143;

async function freePort() {
  const probe = createServer();
  await new Promise((resolve) => probe.listen(0, "127.0.0.1", resolve));
  const { port } = probe.address();
  await new Promise((resolve) => probe.close(resolve));
  return port;
}

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

test("skips without OWNSAND_DATABASE_URL", { timeout: Timeout }, async () => {
  const env = { ...process.env };
  delete env.OWNSAND_DATABASE_URL;
  const run = start(env);
  const { code } = await run.exited;
  assert.equal(code, 0);
  assert.match(run.output(), /ownsand_api_dev_skipped/);
});

test(
  "forwards SIGTERM to the server and drains it",
  { timeout: Timeout, skip: !databaseURL || process.platform === "win32" },
  async () => {
    const port = await freePort();
    const run = start({
      ...process.env,
      ADDR: `127.0.0.1:${port}`,
      OWNSAND_DATABASE_URL: databaseURL,
      OWNSAND_LOGTO_ISSUER: "http://127.0.0.1:9/oidc",
      OWNSAND_STUN_ADDR: "127.0.0.1:0",
    });
    const deadline = Date.now() + 90_000;
    let healthy = false;
    while (Date.now() < deadline && !healthy) {
      try {
        const resp = await fetch(`http://127.0.0.1:${port}/healthz`);
        healthy = resp.ok && (await resp.json()).status === "ok";
      } catch {
        await new Promise((resolve) => setTimeout(resolve, 200));
      }
    }
    assert.ok(healthy, `server did not become healthy:\n${run.output()}`);
    run.child.kill("SIGTERM");
    const { code } = await run.exited;
    assert.equal(code, StatusSigterm, run.output());
    assert.match(run.output(), /server_shutdown_completed/);
  },
);
