// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../..", import.meta.url));
const preflight = join(root, "scripts", "dev", "start-portless-proxy.mjs");

// A fake portless CLI that records its arguments and working directory and
// exits with FAKE_PORTLESS_STATUS.
const fakeCli = `
import { appendFileSync } from "node:fs";
appendFileSync(
  process.env.FAKE_PORTLESS_RECORD,
  JSON.stringify({ args: process.argv.slice(2), cwd: process.cwd() }) + "\\n",
);
process.exit(Number(process.env.FAKE_PORTLESS_STATUS ?? "0"));
`;

// Creates a repository fixture with a copy of the preflight script and the
// given apps. Each app is { name, dev, installed }.
function fixture(t, apps) {
  const dir = mkdtempSync(join(tmpdir(), "portless-preflight-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, "scripts", "dev"), { recursive: true });
  copyFileSync(
    preflight,
    join(dir, "scripts", "dev", "start-portless-proxy.mjs"),
  );
  for (const app of apps) {
    const appDir = join(dir, "apps", app.name);
    mkdirSync(appDir, { recursive: true });
    writeFileSync(
      join(appDir, "package.json"),
      JSON.stringify({ name: app.name, scripts: { dev: app.dev } }),
    );
    if (app.installed) {
      const portlessDir = join(appDir, "node_modules", "portless");
      mkdirSync(join(portlessDir, "dist"), { recursive: true });
      writeFileSync(
        join(portlessDir, "package.json"),
        JSON.stringify({
          name: "portless",
          type: "module",
          bin: { portless: "./dist/cli.js" },
        }),
      );
      writeFileSync(join(portlessDir, "dist", "cli.js"), fakeCli);
    }
  }
  return dir;
}

function run(dir, env = {}) {
  const record = join(dir, "record.jsonl");
  // A PORTLESS value from the developer's shell must not select a branch.
  const { PORTLESS: _shellPortless, ...shellEnv } = process.env;
  const childEnv = { ...shellEnv, FAKE_PORTLESS_RECORD: record, ...env };
  const result = spawnSync(
    process.execPath,
    [join(dir, "scripts", "dev", "start-portless-proxy.mjs")],
    { encoding: "utf8", env: childEnv },
  );
  const calls = existsSync(record)
    ? readFileSync(record, "utf8")
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line))
    : [];
  return { ...result, calls };
}

test("root dev script starts the proxy before turbo", () => {
  const manifest = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  assert.equal(
    manifest.scripts.dev,
    "node scripts/dev/start-portless-proxy.mjs && turbo run dev",
  );
});

test("starts the proxy once with the portless of a portless app", (t) => {
  const dir = fixture(t, [
    { name: "a-native", dev: "rsbuild dev", installed: false },
    { name: "b-web", dev: "portless run --prefix=", installed: true },
    { name: "c-web", dev: "portless run --prefix=", installed: true },
  ]);
  const result = run(dir);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    { args: ["proxy", "start"], cwd: join(dir, "apps", "b-web") },
  ]);
});

for (const value of ["0", "false", "skip"]) {
  test(`does not start the proxy when PORTLESS=${value}`, (t) => {
    const dir = fixture(t, [
      { name: "web", dev: "portless run --prefix=", installed: true },
    ]);
    const result = run(dir, { PORTLESS: value });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, []);
    assert.match(result.stderr, /portless_proxy_preflight_disabled/);
  });
}

test("does nothing when no app runs portless", (t) => {
  const dir = fixture(t, [
    { name: "web", dev: "rsbuild dev", installed: false },
  ]);
  const result = run(dir);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, []);
  assert.match(result.stderr, /portless_proxy_preflight_skipped/);
});

test("fails when portless is not installed", (t) => {
  const dir = fixture(t, [
    { name: "web", dev: "portless run --prefix=", installed: false },
  ]);
  const result = run(dir);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /portless_proxy_start_failed/);
  assert.match(result.stderr, /portless is not installed/);
});

test("exits with the status of a failed proxy start", (t) => {
  const dir = fixture(t, [
    { name: "web", dev: "portless run --prefix=", installed: true },
  ]);
  const result = run(dir, { FAKE_PORTLESS_STATUS: "3" });
  assert.equal(result.status, 3);
  assert.equal(result.calls.length, 1);
  assert.match(result.stderr, /portless_proxy_start_failed/);
});
