// SPDX-License-Identifier: Apache-2.0
// Covers which portless state directory the proxy preflight reads the proxy PID
// from. portless uses `PORTLESS_STATE_DIR` as written, so a relative value
// resolves against the working directory of the portless subprocesses (the app
// directory), not against the directory where the preflight runs.
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

const pinnedVersion = "0.18.1";
const outdatedVersion = "0.15.8";

// The version check reads the proxy command with `ps`, which Windows lacks.
const psSkip = process.platform === "win32" ? "needs ps" : false;

// A fake portless CLI that records its arguments and working directory.
const fakeCli = `
import { appendFileSync } from "node:fs";
appendFileSync(
  process.env.FAKE_PORTLESS_RECORD,
  JSON.stringify({ args: process.argv.slice(2), cwd: process.cwd() }) + "\\n",
);
`;

// A fake proxy process that runs until the test kills it.
const fakeProxyCli = `setInterval(() => {}, 1000);`;

// Creates a repository fixture with one portless app and returns its paths.
function fixture(t) {
  // The real path keeps working directories comparable where the temporary
  // directory is a symbolic link, as on macOS.
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "portless-state-dir-")));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, "scripts", "dev"), { recursive: true });
  copyFileSync(
    preflight,
    join(dir, "scripts", "dev", "start-portless-proxy.mjs"),
  );
  const appDir = join(dir, "apps", "web");
  mkdirSync(appDir, { recursive: true });
  writeFileSync(
    join(appDir, "package.json"),
    JSON.stringify({
      name: "web",
      scripts: { dev: "portless run --prefix=" },
    }),
  );
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
  return { dir, appDir };
}

// Starts a fake proxy of an older portless and records its PID in
// `<stateDir>/proxy.pid`.
function startOutdatedProxy(t, dir, stateDir) {
  const portlessDir = join(dir, "global", "portless");
  mkdirSync(join(portlessDir, "dist"), { recursive: true });
  writeFileSync(
    join(portlessDir, "package.json"),
    JSON.stringify({ name: "portless", version: outdatedVersion }),
  );
  const cli = join(portlessDir, "dist", "cli.js");
  writeFileSync(cli, fakeProxyCli);
  const proxy = spawn(
    process.execPath,
    [cli, "proxy", "start", "--foreground"],
    { stdio: "ignore" },
  );
  t.after(() => proxy.kill());
  mkdirSync(stateDir, { recursive: true });
  writeFileSync(join(stateDir, "proxy.pid"), String(proxy.pid));
  return proxy.pid;
}

// Runs the preflight from the fixture root, as `pnpm dev` does. `stateDir` is
// the value of PORTLESS_STATE_DIR, or undefined to leave it unset; `home` is
// the home directory that the default state directory lives under.
function run(dir, { stateDir, home }) {
  const record = join(dir, "record.jsonl");
  const {
    PORTLESS: _shellPortless,
    PORTLESS_STATE_DIR: _shellStateDir,
    ...shellEnv
  } = process.env;
  const env = {
    ...shellEnv,
    HOME: home,
    USERPROFILE: home,
    FAKE_PORTLESS_RECORD: record,
  };
  if (stateDir !== undefined) env.PORTLESS_STATE_DIR = stateDir;
  const result = spawnSync(
    process.execPath,
    [join(dir, "scripts", "dev", "start-portless-proxy.mjs")],
    { encoding: "utf8", env, cwd: dir },
  );
  const calls = existsSync(record)
    ? readFileSync(record, "utf8")
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line))
    : [];
  return { ...result, calls };
}

function stopThenStart(appDir) {
  return [
    { args: ["proxy", "stop"], cwd: appDir },
    { args: ["proxy", "start"], cwd: appDir },
  ];
}

function startOnly(appDir) {
  return [{ args: ["proxy", "start"], cwd: appDir }];
}

test(
  "resolves a relative state directory against the app directory",
  { skip: psSkip },
  (t) => {
    const { dir, appDir } = fixture(t);
    startOutdatedProxy(t, dir, join(appDir, ".portless-probe"));
    const result = run(dir, { stateDir: ".portless-probe", home: dir });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, stopThenStart(appDir));
    assert.match(result.stderr, /portless_proxy_outdated/);
  },
);

test(
  "does not resolve a relative state directory against the launcher directory",
  { skip: psSkip },
  (t) => {
    const { dir, appDir } = fixture(t);
    // portless would not read this directory: it runs from the app directory.
    startOutdatedProxy(t, dir, join(dir, ".portless-probe"));
    const result = run(dir, { stateDir: ".portless-probe", home: dir });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, startOnly(appDir));
    assert.doesNotMatch(result.stderr, /portless_proxy_outdated/);
  },
);

test("uses an absolute state directory as written", { skip: psSkip }, (t) => {
  const { dir, appDir } = fixture(t);
  const stateDir = join(dir, "absolute-state");
  startOutdatedProxy(t, dir, stateDir);
  const result = run(dir, { stateDir, home: dir });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, stopThenStart(appDir));
});

test(
  "reads ~/.portless when the state directory is unset",
  { skip: psSkip },
  (t) => {
    const { dir, appDir } = fixture(t);
    const home = join(dir, "home");
    startOutdatedProxy(t, dir, join(home, ".portless"));
    const result = run(dir, { home });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, stopThenStart(appDir));
  },
);
