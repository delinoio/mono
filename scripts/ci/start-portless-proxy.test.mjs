// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../..", import.meta.url));
const preflight = join(root, "scripts", "dev", "start-portless-proxy.mjs");

// The portless version that the fixture apps pin.
const pinnedVersion = "0.18.1";

// A fake portless CLI that records its arguments and working directory and
// exits with FAKE_PORTLESS_STOP_STATUS for `proxy stop` and with
// FAKE_PORTLESS_STATUS otherwise.
const fakeCli = `
import { appendFileSync } from "node:fs";
appendFileSync(
  process.env.FAKE_PORTLESS_RECORD,
  JSON.stringify({ args: process.argv.slice(2), cwd: process.cwd() }) + "\\n",
);
const status =
  process.argv[3] === "stop"
    ? process.env.FAKE_PORTLESS_STOP_STATUS
    : process.env.FAKE_PORTLESS_STATUS;
process.exit(Number(status ?? "0"));
`;

// A fake proxy process that runs until the test kills it.
const fakeProxyCli = `setInterval(() => {}, 1000);`;

// Creates a repository fixture with a copy of the preflight script and the
// given apps. Each app is { name, dev, installed }.
function fixture(t, apps) {
  // The real path keeps working directories comparable where the temporary
  // directory is a symbolic link, as on macOS.
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "portless-preflight-")));
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
          version: pinnedVersion,
          type: "module",
          bin: { portless: "./dist/cli.js" },
        }),
      );
      writeFileSync(join(portlessDir, "dist", "cli.js"), fakeCli);
    }
  }
  return dir;
}

// Starts a fake proxy from a portless package of the given version and
// records its PID in the fixture's portless state directory.
function startProxy(t, dir, version) {
  const portlessDir = join(dir, "global", "portless");
  mkdirSync(join(portlessDir, "dist"), { recursive: true });
  writeFileSync(
    join(portlessDir, "package.json"),
    JSON.stringify({ name: "portless", version }),
  );
  const cli = join(portlessDir, "dist", "cli.js");
  writeFileSync(cli, fakeProxyCli);
  const proxy = spawn(
    process.execPath,
    [cli, "proxy", "start", "--foreground"],
    { stdio: "ignore" },
  );
  t.after(() => proxy.kill());
  mkdirSync(join(dir, "state"), { recursive: true });
  writeFileSync(join(dir, "state", "proxy.pid"), String(proxy.pid));
  return proxy.pid;
}

function run(dir, env = {}) {
  const record = join(dir, "record.jsonl");
  // A PORTLESS value from the developer's shell must not select a branch.
  const { PORTLESS: _shellPortless, ...shellEnv } = process.env;
  // The proxy of the developer's machine must not select a branch either.
  const childEnv = {
    ...shellEnv,
    PORTLESS_STATE_DIR: join(dir, "state"),
    FAKE_PORTLESS_RECORD: record,
    ...env,
  };
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

test("root dev script uses the shared development launcher", () => {
  const manifest = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  assert.equal(manifest.scripts.dev, "node scripts/dev/start-dev.mjs");
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

test("finds a portless app behind the Infisical development launcher", async (t) => {
  for (const dev of [
    "node ../../scripts/dev/with-infisical.mjs --path=/web -- node node_modules/portless/dist/cli.js run --prefix=",
    "node ../../scripts/dev/with-infisical.mjs --path=/web -- portless run --prefix=",
    "node node_modules/portless/dist/cli.js run --prefix=",
  ]) {
    await t.test(dev, (t) => {
      const dir = fixture(t, [
        { name: "a-native", dev: "rsbuild dev", installed: false },
        { name: "b-web", dev, installed: true },
      ]);
      const result = run(dir);
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(result.calls, [
        { args: ["proxy", "start"], cwd: join(dir, "apps", "b-web") },
      ]);
    });
  }
  // The launcher in front of a command other than portless is not a portless
  // app.
  const dir = fixture(t, [
    {
      name: "api",
      dev: "node ../../scripts/dev/with-infisical.mjs --path=/api -- node scripts/dev.mjs",
      installed: true,
    },
  ]);
  const result = run(dir);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, []);
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

// The version check reads the proxy command with `ps`, which Windows lacks.
const psSkip = process.platform === "win32" ? "needs ps" : false;

test("stops a proxy that an older portless runs", { skip: psSkip }, (t) => {
  const dir = fixture(t, [
    { name: "web", dev: "portless run --prefix=", installed: true },
  ]);
  const pid = startProxy(t, dir, "0.15.8");
  const result = run(dir);
  assert.equal(result.status, 0, result.stderr);
  const cwd = join(dir, "apps", "web");
  assert.deepEqual(result.calls, [
    { args: ["proxy", "stop"], cwd },
    { args: ["proxy", "start"], cwd },
  ]);
  const outdated = result.stderr
    .split("\n")
    .filter((line) => line.includes("portless_proxy_outdated"))
    .map((line) => JSON.parse(line));
  assert.deepEqual(outdated, [
    {
      event: "portless_proxy_outdated",
      pid,
      runningVersion: "0.15.8",
      pinnedVersion,
    },
  ]);
});

for (const version of [pinnedVersion, "0.19.0"]) {
  test(`keeps a proxy that portless ${version} runs`, { skip: psSkip }, (t) => {
    const dir = fixture(t, [
      { name: "web", dev: "portless run --prefix=", installed: true },
    ]);
    startProxy(t, dir, version);
    const result = run(dir);
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, [
      { args: ["proxy", "start"], cwd: join(dir, "apps", "web") },
    ]);
    assert.doesNotMatch(result.stderr, /portless_proxy_outdated/);
  });
}

test("ignores a recorded PID of a process that is not portless", (t) => {
  const dir = fixture(t, [
    { name: "web", dev: "portless run --prefix=", installed: true },
  ]);
  mkdirSync(join(dir, "state"), { recursive: true });
  writeFileSync(join(dir, "state", "proxy.pid"), String(process.pid));
  const result = run(dir);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    { args: ["proxy", "start"], cwd: join(dir, "apps", "web") },
  ]);
});

test(
  "fails without a start when the outdated proxy does not stop",
  {
    skip: psSkip,
  },
  (t) => {
    const dir = fixture(t, [
      { name: "web", dev: "portless run --prefix=", installed: true },
    ]);
    startProxy(t, dir, "0.15.8");
    const result = run(dir, { FAKE_PORTLESS_STOP_STATUS: "4" });
    assert.equal(result.status, 4);
    assert.deepEqual(result.calls, [
      { args: ["proxy", "stop"], cwd: join(dir, "apps", "web") },
    ]);
    assert.match(result.stderr, /portless_proxy_stop_failed/);
  },
);
