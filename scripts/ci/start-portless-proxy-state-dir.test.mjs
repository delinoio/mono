// SPDX-License-Identifier: Apache-2.0
// Covers state directory selection and propagation to the proxy and app tasks.
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, sep } from "node:path";
import { createInterface } from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { spawnProcessGroup } from "./fixture-process.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const preflight = "start-portless-proxy.mjs";
const launcher = "start-dev.mjs";
const appNames = ["web", "web-sibling"];

const pinnedVersion = "0.18.1";
const outdatedVersion = "0.15.8";

// The version check reads the proxy command with `ps`, which Windows lacks.
const psSkip = process.platform === "win32" ? "needs ps" : false;

// A fake portless CLI that records its arguments, cwd and state override.
const fakeCli = `
import { appendFileSync } from "node:fs";
const args = process.argv.slice(2);
appendFileSync(
  process.env.FAKE_PORTLESS_RECORD,
  JSON.stringify({
    args,
    cwd: process.cwd(),
    stateDir: process.env.PORTLESS_STATE_DIR ?? null,
  }) + "\\n",
);
if (args[0] === "proxy") process.exit(Number(process.env.FAKE_PORTLESS_STATUS));
`;

// A fake Turbo CLI that runs Portless from each task's working directory.
const fakeTurbo = `
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
assert.deepEqual(process.argv.slice(2), ["run", "dev"]);
for (const name of ${JSON.stringify(appNames)}) {
  const result = spawnSync(
    process.execPath,
    [join("node_modules", "portless", "dist", "cli.js"), "run"],
    { cwd: join(process.cwd(), "apps", name), stdio: "inherit" },
  );
  assert.equal(result.status, 0);
}
`;

// A fake Turbo CLI that waits for the test to close stdin after a signal.
const signalCountingTurbo = `
import assert from "node:assert/strict";
assert.deepEqual(process.argv.slice(2), ["run", "dev", "--filter=web"]);
assert.equal(process.env.PORTLESS, "0");
for (const signal of ["SIGTERM", "SIGINT"]) {
  process.on(signal, () => console.log(signal));
}
process.stdin.on("end", () => { process.exitCode = 42; });
process.stdin.resume();
console.log("ready");
`;

// A fake proxy process that runs until the test kills it.
const fakeProxyCli = `setInterval(() => {}, 1000);`;

// Keep launcher fixtures independent of the developer's listener on port 443.
const fakeProbe = `
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import http from "node:http";
import { syncBuiltinESMExports } from "node:module";
import { dirname, sep } from "node:path";
import { fileURLToPath } from "node:url";
const fixtureRoot = dirname(fileURLToPath(import.meta.url));
const lstat = fs.lstatSync;
fs.lstatSync = (path) => {
  const stat = lstat(path);
  // TMPDIR can live in a group-writable checkout. Only fixture-owned paths
  // use their actual permissions in these tests.
  if (fixtureRoot.startsWith(path + sep)) stat.mode &= ~0o022;
  return stat;
};
http.request = (options, onResponse) => {
  assert.equal(options.hostname, "127.0.0.1");
  assert.equal(options.port, 443);
  assert.equal(options.method, "HEAD");
  const socket = new EventEmitter();
  socket.destroy = () => {};
  socket.end = () => queueMicrotask(() => {
    const probe = process.env.FAKE_PORTLESS_PROBE;
    if (probe === "refused") {
      socket.emit("error", new Error("ECONNREFUSED"));
    } else {
      onResponse({ headers: probe === "portless" ? { "x-portless": "1" } : {} });
    }
  });
  return socket;
};
syncBuiltinESMExports();
`;

// Creates a repository fixture with two Portless apps and a Turbo CLI.
function fixture(t) {
  // The real path keeps working directories comparable where the temporary
  // directory is a symbolic link, as on macOS.
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "portless-state-dir-")));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, "scripts", "dev"), { recursive: true });
  mkdirSync(join(dir, "scripts", "ci"), { recursive: true });
  copyFileSync(
    join(root, "scripts", "ci", "workspace-node.mjs"),
    join(dir, "scripts", "ci", "workspace-node.mjs"),
  );
  for (const script of [preflight, launcher]) {
    copyFileSync(
      join(root, "scripts", "dev", script),
      join(dir, "scripts", "dev", script),
    );
  }
  writeFileSync(join(dir, "probe.mjs"), fakeProbe);
  const turboDir = join(dir, "node_modules", "turbo");
  mkdirSync(turboDir, { recursive: true });
  writeFileSync(
    join(turboDir, "package.json"),
    JSON.stringify({ bin: { turbo: "./cli.mjs" } }),
  );
  writeFileSync(join(turboDir, "cli.mjs"), fakeTurbo);
  const [appDir, siblingDir] = appNames.map((name) => addFakeApp(dir, name));
  return { dir, appDir, siblingDir };
}

function proxyCliPath(dir) {
  return join(dir, "global", "portless", "dist", "cli.js");
}

function proxyCliPathHasWhitespace(dir) {
  return /\s/.test(proxyCliPath(dir));
}

function versionedProxyFixture(t) {
  const repo = fixture(t);
  // Preflight splits the ps command on whitespace. Keep these PID checks
  // skipped for spaced CLI paths until preflight can detect their versions.
  if (proxyCliPathHasWhitespace(repo.dir)) {
    t.skip("whitespace in the proxy CLI path prevents version detection");
    return null;
  }
  return repo;
}

function addFakeApp(dir, name) {
  const appDir = join(dir, "apps", name);
  mkdirSync(appDir, { recursive: true });
  writeFileSync(
    join(appDir, "package.json"),
    JSON.stringify({
      name,
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
  return appDir;
}

// Starts a fake proxy of an older portless and records its PID in
// `<stateDir>/proxy.pid`.
function startOutdatedProxy(t, dir, stateDir) {
  const cli = proxyCliPath(dir);
  const dist = dirname(cli);
  mkdirSync(dist, { recursive: true });
  writeFileSync(
    join(dist, "..", "package.json"),
    JSON.stringify({ name: "portless", version: outdatedVersion }),
  );
  writeFileSync(cli, fakeProxyCli);
  const proxy = spawn(
    process.execPath,
    [cli, "proxy", "start", "--foreground"],
    { stdio: "ignore" },
  );
  t.after(() => proxy.kill());
  mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  writeFileSync(join(stateDir, "proxy.pid"), String(proxy.pid), {
    mode: 0o600,
  });
  return proxy.pid;
}

// Runs the requested entry point from the fixture root.
function run(
  dir,
  {
    stateDir,
    home = dir,
    script = preflight,
    portless,
    proxyStatus = 0,
    probe = "refused",
    mode,
  },
) {
  const record = join(dir, "record.jsonl");
  const {
    PORTLESS: _shellPortless,
    PORTLESS_HTTPS: _shellHttps,
    PORTLESS_PORT: _shellPort,
    PORTLESS_STATE_DIR: _shellStateDir,
    PORTLESS_UNPRIVILEGED: _shellUnprivileged,
    ...shellEnv
  } = process.env;
  const env = {
    ...shellEnv,
    HOME: home,
    USERPROFILE: home,
    FAKE_PORTLESS_RECORD: record,
    FAKE_PORTLESS_STATUS: String(proxyStatus),
    FAKE_PORTLESS_PROBE: probe,
    ...mode,
  };
  if (portless !== undefined) env.PORTLESS = portless;
  if (stateDir !== undefined) env.PORTLESS_STATE_DIR = stateDir;
  const result = spawnSync(
    process.execPath,
    ["--import", join(dir, "probe.mjs"), join(dir, "scripts", "dev", script)],
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

function stopThenStart(appDir, stateDir = null) {
  return [
    { args: ["proxy", "stop"], cwd: appDir, stateDir },
    ...startOnly(appDir, stateDir),
  ];
}

function startOnly(appDir, stateDir = null) {
  return [{ args: ["proxy", "start"], cwd: appDir, stateDir }];
}

test(
  "resolves a relative state directory against the app directory",
  { skip: psSkip },
  (t) => {
    const repo = versionedProxyFixture(t);
    if (repo === null) return;
    const { dir, appDir } = repo;
    const stateDir = join(appDir, ".portless-probe");
    startOutdatedProxy(t, dir, stateDir);
    const result = run(dir, { stateDir: ".portless-probe", home: dir });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, stopThenStart(appDir, stateDir));
    assert.match(result.stderr, /portless_proxy_outdated/);
  },
);

test(
  "does not resolve a relative state directory against the launcher directory",
  { skip: psSkip },
  (t) => {
    const repo = versionedProxyFixture(t);
    if (repo === null) return;
    const { dir, appDir } = repo;
    // portless would not read this directory: it runs from the app directory.
    startOutdatedProxy(t, dir, join(dir, ".portless-probe"));
    const result = run(dir, { stateDir: ".portless-probe", home: dir });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(
      result.calls,
      startOnly(appDir, join(appDir, ".portless-probe")),
    );
    assert.doesNotMatch(result.stderr, /portless_proxy_outdated/);
  },
);

test("uses an absolute state directory as written", { skip: psSkip }, (t) => {
  const repo = versionedProxyFixture(t);
  if (repo === null) return;
  const { dir, appDir } = repo;
  const stateDir = join(dir, "absolute-state");
  startOutdatedProxy(t, dir, stateDir);
  const result = run(dir, { stateDir, home: dir });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, stopThenStart(appDir, stateDir));
});

test(
  "reads ~/.portless when the state directory is unset",
  { skip: psSkip },
  (t) => {
    const repo = versionedProxyFixture(t);
    if (repo === null) return;
    const { dir, appDir } = repo;
    const home = join(dir, "home");
    startOutdatedProxy(t, dir, join(home, ".portless"));
    const result = run(dir, { home });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.calls, stopThenStart(appDir));
  },
);

function appCalls(appDir, siblingDir, stateDir) {
  return [appDir, siblingDir].map((cwd) => ({ args: ["run"], cwd, stateDir }));
}

test("root dev shares the preflight app's relative state directory", (t) => {
  const { dir, appDir, siblingDir } = fixture(t);
  const stateDir = join(appDir, ".portless-probe");
  if (!psSkip) startOutdatedProxy(t, dir, stateDir);
  const detectsOutdatedProxy = !psSkip && !proxyCliPathHasWhitespace(dir);
  const result = run(dir, { script: launcher, stateDir: ".portless-probe" });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    ...(detectsOutdatedProxy
      ? stopThenStart(appDir, stateDir)
      : startOnly(appDir, stateDir)),
    ...appCalls(appDir, siblingDir, stateDir),
  ]);
  if (detectsOutdatedProxy) {
    assert.match(result.stderr, /portless_proxy_outdated/);
  } else {
    assert.doesNotMatch(result.stderr, /portless_proxy_outdated/);
  }
});

test("root dev passes the absolute override through as supplied", (t) => {
  const { dir, appDir, siblingDir } = fixture(t);
  const stateDir = join(dir, "absolute-state") + sep + ".";
  const result = run(dir, { script: launcher, stateDir });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    ...startOnly(appDir, stateDir),
    ...appCalls(appDir, siblingDir, stateDir),
  ]);
});

test("root dev leaves the state directory unset by default", (t) => {
  const { dir, appDir, siblingDir } = fixture(t);
  const result = run(dir, { script: launcher });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    ...startOnly(appDir),
    ...appCalls(appDir, siblingDir, null),
  ]);
});

test("root dev exits before Turbo when the proxy preflight fails", (t) => {
  const { dir, appDir } = fixture(t);
  const result = run(dir, { script: launcher, proxyStatus: 3 });
  assert.equal(result.status, 3, result.stderr);
  assert.deepEqual(result.calls, startOnly(appDir));
  assert.match(result.stderr, /portless_proxy_start_failed/);
});

test("root dev starts Turbo without a proxy when PORTLESS=0", (t) => {
  const { dir, appDir, siblingDir } = fixture(t);
  const result = run(dir, {
    script: launcher,
    portless: "0",
    probe: "portless",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, appCalls(appDir, siblingDir, null));
  assert.match(result.stderr, /portless_proxy_preflight_disabled/);
});

test(
  "root dev rejects a Portless listener with no matching default state",
  { skip: psSkip },
  async (t) => {
    for (const stateDir of [undefined, ""]) {
      await t.test(
        stateDir === undefined ? "unset override" : "empty override",
        (t) => {
          const { dir } = fixture(t);
          const result = run(dir, {
            stateDir,
            script: launcher,
            probe: "portless",
          });
          assert.equal(result.status, 1, result.stderr);
          assert.deepEqual(result.calls, []);
          assert.deepEqual(JSON.parse(result.stderr), {
            event: "portless_proxy_state_mismatch",
            address: "127.0.0.1",
            port: 443,
            expectedStateDir: join(dir, ".portless"),
            hint: "Stop the proxy with `portless proxy stop` from the Portless installation that started it, then rerun `pnpm dev`.",
          });
        },
      );
    }
  },
);

test(
  "root dev ignores PID files from writable or linked default state",
  { skip: psSkip },
  async (t) => {
    for (const location of ["directory", "directory symlink", "PID symlink"]) {
      await t.test(location, (t) => {
        const { dir } = fixture(t);
        const stateDir = join(dir, ".portless");
        const legacyDir =
          location === "directory" ? stateDir : join(dir, "legacy");
        const pid = startOutdatedProxy(t, dir, legacyDir);
        chmodSync(legacyDir, 0o1777);
        const legacyPid = join(legacyDir, "proxy.pid");
        const legacyRoutes = join(legacyDir, "routes.json");
        writeFileSync(legacyRoutes, "legacy routes sentinel");
        if (location === "directory symlink") symlinkSync(legacyDir, stateDir);
        if (location === "PID symlink") {
          mkdirSync(stateDir);
          symlinkSync(legacyPid, join(stateDir, "proxy.pid"));
        }

        const result = run(dir, { script: launcher, probe: "portless" });
        assert.equal(result.status, 1, result.stderr);
        assert.equal(
          JSON.parse(result.stderr).event,
          "portless_proxy_state_mismatch",
        );
        assert.deepEqual(result.calls, []);
        assert.equal(readFileSync(legacyPid, "utf8"), String(pid));
        assert.equal(
          readFileSync(legacyRoutes, "utf8"),
          "legacy routes sentinel",
        );
        assert.doesNotThrow(() => process.kill(pid, 0));
      });
    }
  },
);

test(
  "root dev checks the recorded listener port and process start time",
  { skip: psSkip },
  async (t) => {
    for (const record of ["matching", "wrong port", "stale start time"]) {
      await t.test(record, (t) => {
        const repo = versionedProxyFixture(t);
        if (repo === null) return;
        const { dir, appDir, siblingDir } = repo;
        const stateDir = join(dir, ".portless");
        const pid = startOutdatedProxy(t, dir, stateDir);
        const start =
          process.platform === "linux"
            ? /^\d+ \(.*\) (?:\S+ ){19}(\d+)/.exec(
                readFileSync(`/proc/${pid}/stat`, "utf8"),
              )[1]
            : spawnSync("ps", ["-o", "lstart=", "-p", String(pid)], {
                encoding: "utf8",
                env: { ...process.env, LC_ALL: "C", TZ: "UTC" },
              })
                .stdout.trim()
                .replace(/\s+/g, " ");
        writeFileSync(
          join(stateDir, "proxy.port"),
          record === "wrong port" ? "80" : "443",
          { mode: 0o600 },
        );
        writeFileSync(
          join(stateDir, "proxy.start"),
          record === "stale start time" ? "stale" : start,
          { mode: 0o600 },
        );

        const result = run(dir, { script: launcher, probe: "portless" });
        if (record === "matching") {
          assert.equal(result.status, 0, result.stderr);
          assert.deepEqual(result.calls, [
            ...stopThenStart(appDir),
            ...appCalls(appDir, siblingDir, null),
          ]);
        } else {
          assert.equal(result.status, 1, result.stderr);
          assert.equal(
            JSON.parse(result.stderr).event,
            "portless_proxy_state_mismatch",
          );
          assert.deepEqual(result.calls, []);
        }
      });
    }
  },
);

test("root dev keeps the pinned startup path for unrelated listeners and overrides", async (t) => {
  for (const [name, config] of [
    ["unrelated listener", { probe: "unrelated" }],
    ["state override", { stateDir: "override", probe: "portless" }],
    ["custom port", { mode: { PORTLESS_PORT: "1355" }, probe: "portless" }],
    ["plain HTTP", { mode: { PORTLESS_HTTPS: "0" }, probe: "portless" }],
    [
      "unprivileged HTTP",
      { mode: { PORTLESS_UNPRIVILEGED: "1" }, probe: "portless" },
    ],
  ]) {
    await t.test(name, (t) => {
      const { dir, appDir, siblingDir } = fixture(t);
      const result = run(dir, { script: launcher, ...config });
      const stateDir = config.stateDir ? join(appDir, config.stateDir) : null;
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(result.calls, [
        ...startOnly(appDir, stateDir),
        ...appCalls(appDir, siblingDir, stateDir),
      ]);
    });
  }
});

for (const { signal, group } of [
  { signal: "SIGTERM", group: false },
  { signal: "SIGINT", group: true },
]) {
  test(
    `root dev waits for Turbo after ${group ? "group" : "PID-directed"} ${signal}`,
    {
      skip: process.platform === "win32" && "needs POSIX signals",
      timeout: 5000,
    },
    async (t) => {
      const { dir } = fixture(t);
      writeFileSync(
        join(dir, "node_modules", "turbo", "cli.mjs"),
        signalCountingTurbo,
      );
      const { child, done } = spawnProcessGroup(
        t,
        process.execPath,
        [join(dir, "scripts", "dev", launcher), "--filter=web"],
        { cwd: dir, env: { ...process.env, PORTLESS: "0" } },
      );
      const lines = createInterface({ input: child.stdout });
      const options = { signal: t.signal };
      assert.deepEqual(await once(lines, "line", options), ["ready"]);

      process.kill(group ? -child.pid : child.pid, signal);
      assert.deepEqual(await once(lines, "line", options), [signal]);
      assert.equal(child.exitCode, null);
      assert.equal(child.signalCode, null);

      child.stdin.end();
      const result = await done;
      assert.equal(result.status, 42, result.stderr);
      assert.equal(result.stdout, `ready\n${signal}\n`);
    },
  );
}
