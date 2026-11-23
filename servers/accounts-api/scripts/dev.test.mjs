// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";

// These tests start the real launcher, which builds and runs the real server.
// They use POSIX signals and process groups, so Windows is skipped like the
// formatter signal tests.
const skip = process.platform === "win32";
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const StatusSigint = 130;
const StatusSigterm = 143;
const StatusSigquit = 131;
const Timeout = 60_000;
// The deadline of waitFor. It must stay below the smallest per-test timeout
// (Timeout) minus the setup time that runs before the first wait (port
// reservation and launcher start). The node:test timer starts with the test, so
// an equal deadline lets the runner fail the test first with a bare "test timed
// out" and the stdout/stderr diagnostic of waitFor is never printed.
const WaitTimeout = 45_000;

// Reserves a free loopback port number. The port is released before returning,
// so a launcher started right after can bind it.
async function freePort() {
  const probe = createServer();
  await new Promise((resolve) => probe.listen(0, "127.0.0.1", resolve));
  const { port } = probe.address();
  await new Promise((resolve) => probe.close(resolve));
  return port;
}

// Accepts connections on port until closed; used to occupy a port.
async function occupy(port) {
  const holder = createServer();
  await new Promise((resolve, reject) => {
    holder.once("error", reject);
    holder.listen(port, "127.0.0.1", resolve);
  });
  return holder;
}

// Resolves true when binding the port fails, which means a listener holds it.
function portInUse(port) {
  return new Promise((resolve) => {
    const probe = createServer();
    probe.once("error", () => resolve(true));
    probe.listen(port, "127.0.0.1", () => probe.close(() => resolve(false)));
  });
}

// Lists the direct children of a process through pgrep (procps on Linux, a
// system binary on macOS), so no dependency is added. pgrep exits 1 when there
// is no match and also fails when the process is gone; both mean "no children".
function childPids(pid) {
  try {
    return execFileSync("pgrep", ["-P", String(pid)], { encoding: "utf8" })
      .split("\n")
      .filter(Boolean)
      .map(Number);
  } catch {
    return [];
  }
}

// Reports whether a process still runs. A killed orphan that its new parent has
// not reaped yet is a zombie: it holds no port and is not running, so it counts
// as gone.
function isRunning(pid) {
  try {
    const state = execFileSync("ps", ["-o", "stat=", "-p", String(pid)], {
      encoding: "utf8",
    }).trim();
    return state !== "" && !state.startsWith("Z");
  } catch {
    return false;
  }
}

// Remembers the launcher's current children (the supervisor of the build or the
// server) and their children (the build or the server). Call it while the
// launcher is alive: once the launcher dies, its children are reparented and
// `pgrep -P <launcher pid>` no longer finds them.
function recordChildren(run) {
  for (const pid of childPids(run.child.pid)) {
    run.childPids.add(pid);
    for (const grandchild of childPids(pid)) run.childPids.add(grandchild);
  }
}

// Kills everything a launcher test may have left behind. The launcher runs each
// child (the build and the server) under a supervisor in a process group of its
// own (see dev.mjs), so signalling the launcher's group never reaches them; the
// recorded pids and their groups are killed instead. The supervisor leads its
// group, so killing the group of a recorded pid ends the supervisor and the
// server together (for the server's pid the group kill finds no group and the
// pid kill ends it). Children go first, while the launcher is still alive. The
// pids are recorded moments earlier in the same test, so a pid reused by an
// unrelated process is not a practical risk.
function stopLauncher(run) {
  recordChildren(run);
  for (const pid of run.childPids) {
    for (const target of [-pid, pid]) {
      try {
        process.kill(target, "SIGKILL");
      } catch {
        // The process or its group is already gone.
      }
    }
  }
  try {
    process.kill(-run.child.pid, "SIGKILL");
  } catch {
    // The launcher's group is already gone.
  }
  rmSync(run.tmp, { recursive: true, force: true });
}

// Starts the launcher in its own process group so a test can signal the group
// of the launcher itself. The launcher's children are not in that group, so
// cleanup (`stopLauncher`) kills them by pid and group. The dev-forced-stop
// tests cover a launcher that dies. The temporary build output goes
// to a test-owned TMPDIR.
// Configuration that lets the real server start without external services.
// The server opens its database pool lazily and skips the migrations with
// RUN_MIGRATIONS=false, so the unreachable database (port 1 refuses
// connections) is never contacted: these tests cover the launcher and the
// process lifecycle, not the billing APIs, which the Go integration tests
// cover with a real Postgres. Logto, the Logto Management API, Resend and
// Paddle are contacted only by RPCs and by billing work found in the
// database, which these tests do not reach.
const serverEnv = Object.freeze({
  DATABASE_URL: "postgres://accounts@127.0.0.1:1/accounts?connect_timeout=1",
  RUN_MIGRATIONS: "false",
  LOGTO_ISSUER: "http://127.0.0.1:1/oidc",
  LOGTO_MANAGEMENT_ENDPOINT: "http://127.0.0.1:1",
  LOGTO_M2M_CLIENT_ID: "launcher-test",
  LOGTO_M2M_CLIENT_SECRET: "launcher-test-secret",
  RESEND_API_KEY: "re_launcher_test",
  EMAIL_FROM: "billing@example.com",
  PADDLE_ENVIRONMENT: "sandbox",
  PADDLE_API_KEY: "pdl_launcher_test",
  PADDLE_WEBHOOK_SECRET: "pdl_ntfset_launcher_test",
  PADDLE_SUBSCRIPTION_PRICE_ID: "pri_01aaaaaaaaaaaaaaaaaaaaaaaa",
  PADDLE_TOP_UP_PRODUCT_ID: "pro_01bbbbbbbbbbbbbbbbbbbbbbbb",
});

function startLauncher(t, { port, env = {}, pathPrefix } = {}) {
  const tmp = mkdtempSync(join(tmpdir(), "accounts-api-dev-test-"));
  const childEnv = {
    ...process.env,
    ...serverEnv,
    ADDR: `127.0.0.1:${port}`,
    TMPDIR: tmp,
  };
  delete childEnv.LOG_LEVEL;
  // Settings the server validates or binds at startup must not leak in from the
  // environment that runs the tests: an occupied inherited METRICS_ADDR makes
  // the server exit before server_started, and an invalid inherited
  // CORS_ORIGINS or WEB_ORIGIN is rejected. A blank METRICS_ADDR disables the
  // metrics listener, and an unset origin selects the launcher default. A test
  // can still set or unset any of them through `env`, which is applied next.
  childEnv.METRICS_ADDR = "";
  delete childEnv.CORS_ORIGINS;
  delete childEnv.WEB_ORIGIN;
  Object.assign(childEnv, env);
  for (const [name, value] of Object.entries(childEnv)) {
    if (value === undefined) delete childEnv[name];
  }
  if (pathPrefix) {
    childEnv.PATH = `${pathPrefix}${delimiter}${process.env.PATH}`;
  }
  const child = spawn(process.execPath, [launcher], {
    env: childEnv,
    stdio: ["ignore", "pipe", "pipe"],
    detached: true,
  });
  const run = { child, tmp, stdout: "", stderr: "", childPids: new Set() };
  child.stdout.on("data", (chunk) => (run.stdout += chunk));
  child.stderr.on("data", (chunk) => (run.stderr += chunk));
  run.exited = new Promise((resolve) =>
    child.once("close", (code, signal) => resolve({ code, signal })),
  );
  t.after(() => stopLauncher(run));
  return run;
}

async function waitFor(run, predicate, what) {
  const deadline = Date.now() + WaitTimeout;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  assert.fail(
    `timed out waiting for ${what}\nstdout: ${run.stdout}\nstderr: ${run.stderr}`,
  );
}

// Waits for the server to run and records the launcher's children while the
// launcher is alive, so cleanup can still find the server after a kill.
async function waitReady(run) {
  await waitFor(
    run,
    () => run.stdout.includes('"server_started"'),
    "server_started",
  );
  recordChildren(run);
}

// The server must be gone and the port reusable once the launcher has exited.
async function assertNoListener(port) {
  assert.equal(await portInUse(port), false, "a listener survived");
}

function fakeGo(t, script) {
  const dir = mkdtempSync(join(tmpdir(), "accounts-api-fake-go-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "go");
  writeFileSync(file, `#!/bin/sh\n${script}\n`);
  chmodSync(file, 0o755);
  return dir;
}

// A fake `go` whose "build" writes a signal-counting program to the -o path.
// The program mirrors the server's signal contract: the first SIGINT starts a
// drain that ends after DrainMs with server_shutdown_completed; a second SIGINT
// ends it at once without that event. Every SIGINT prints a child_sigint event.
const DrainMs = 1500;
const countingServer = `#!${process.execPath}
let sigints = 0;
process.on("SIGINT", () => {
  sigints += 1;
  console.log(JSON.stringify({ event: "child_sigint", n: sigints }));
  if (sigints > 1) process.exit(2);
  setTimeout(() => {
    console.log(JSON.stringify({ event: "server_shutdown_completed" }));
    process.exit(0);
  }, ${DrainMs});
});
console.log(JSON.stringify({ event: "server_started" }));
setInterval(() => {}, 1000);
`;

function fakeGoBuildingCountingServer(t) {
  return fakeGo(
    t,
    [
      'while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done',
      `cat > "$out" <<'PROGRAM'\n${countingServer}PROGRAM`,
      'chmod 755 "$out"',
    ].join("\n"),
  );
}

const countEvents = (output, event) =>
  output.split("\n").filter((line) => line.includes(`"${event}"`)).length;

test(
  "skips without DATABASE_URL and starts no build",
  { skip, timeout: Timeout },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, {
      port,
      env: { DATABASE_URL: undefined },
      // A build would fail with this status; the launcher must not run it.
      pathPrefix: fakeGo(t, "exit 3"),
    });
    const { code } = await run.exited;

    assert.equal(code, 0, run.stderr);
    assert.match(run.stderr, /"accounts_api_dev_skipped"/);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "a build directory was created");
  },
);

test(
  "ASCII white space only in DATABASE_URL counts as unset",
  { skip, timeout: Timeout },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, {
      port,
      env: { DATABASE_URL: " \t\n\v\f\r " },
      pathPrefix: fakeGo(t, "exit 3"),
    });
    const { code } = await run.exited;

    assert.equal(code, 0, run.stderr);
    assert.match(run.stderr, /"accounts_api_dev_skipped"/);
    assert.deepEqual(readdirSync(run.tmp), [], "a build directory was created");
  },
);

// The server removes only ASCII white space, so Unicode white space is a value
// that the server rejects, not an unset one: the launcher must build and start
// the server instead of skipping.
test(
  "Unicode white space only in DATABASE_URL is not unset",
  { skip, timeout: Timeout },
  async (t) => {
    const run = startLauncher(t, {
      port: await freePort(),
      env: { DATABASE_URL: "\u00a0" },
      // The build fails with a distinct status that proves it ran.
      pathPrefix: fakeGo(t, "exit 3"),
    });
    const { code } = await run.exited;

    assert.equal(code, 3, run.stderr);
    assert.doesNotMatch(run.stderr, /"accounts_api_dev_skipped"/);
  },
);

// A fake `go` whose "build" writes a program that prints the origins it
// received and exits.
function fakeGoBuildingEnvPrinter(t) {
  const program = `#!${process.execPath}
console.log(JSON.stringify({ event: "env", cors: process.env.CORS_ORIGINS, web: process.env.WEB_ORIGIN }));
`;
  return fakeGo(
    t,
    [
      'while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done',
      `cat > "$out" <<'PROGRAM'\n${program}PROGRAM`,
      'chmod 755 "$out"',
    ].join("\n"),
  );
}

const envRecord = (output) =>
  JSON.parse(output.split("\n").find((line) => line.includes('"event":"env"')));

test(
  "development origins are the defaults and explicit values win",
  { skip, timeout: Timeout },
  async (t) => {
    const defaults = startLauncher(t, {
      port: await freePort(),
      env: { CORS_ORIGINS: undefined, WEB_ORIGIN: undefined },
      pathPrefix: fakeGoBuildingEnvPrinter(t),
    });
    assert.equal((await defaults.exited).code, 0, defaults.stderr);
    assert.deepEqual(envRecord(defaults.stdout), {
      event: "env",
      cors: "https://accounts.delino.io,http://127.0.0.1:13100,https://accounts.delino.localhost,http://accounts.delino.localhost",
      web: "https://accounts.delino.localhost",
    });

    const explicit = startLauncher(t, {
      port: await freePort(),
      env: {
        CORS_ORIGINS: "https://a.example",
        WEB_ORIGIN: "https://b.example",
      },
      pathPrefix: fakeGoBuildingEnvPrinter(t),
    });
    assert.equal((await explicit.exited).code, 0, explicit.stderr);
    assert.deepEqual(envRecord(explicit.stdout), {
      event: "env",
      cors: "https://a.example",
      web: "https://b.example",
    });
  },
);

test(
  "ASCII white space only CORS_ORIGINS and WEB_ORIGIN select the defaults",
  { skip, timeout: Timeout },
  async (t) => {
    const run = startLauncher(t, {
      port: await freePort(),
      env: { CORS_ORIGINS: " \t\n\v\f\r ", WEB_ORIGIN: "\r\n" },
      pathPrefix: fakeGoBuildingEnvPrinter(t),
    });
    assert.equal((await run.exited).code, 0, run.stderr);
    assert.deepEqual(envRecord(run.stdout), {
      event: "env",
      cors: "https://accounts.delino.io,http://127.0.0.1:13100,https://accounts.delino.localhost,http://accounts.delino.localhost",
      web: "https://accounts.delino.localhost",
    });
  },
);

test(
  "Unicode white space only CORS_ORIGINS and WEB_ORIGIN reach the server unchanged",
  { skip, timeout: Timeout },
  async (t) => {
    const run = startLauncher(t, {
      port: await freePort(),
      env: { CORS_ORIGINS: "\u00a0", WEB_ORIGIN: "\u3000" },
      pathPrefix: fakeGoBuildingEnvPrinter(t),
    });
    assert.equal((await run.exited).code, 0, run.stderr);
    assert.deepEqual(envRecord(run.stdout), {
      event: "env",
      cors: "\u00a0",
      web: "\u3000",
    });
  },
);

test(
  "a process-group SIGINT reaches the server once and the drain completes",
  { skip, timeout: Timeout },
  async (t) => {
    const run = startLauncher(t, {
      port: await freePort(),
      pathPrefix: fakeGoBuildingCountingServer(t),
    });
    await waitReady(run);

    // A terminal Ctrl-C signals every process of the foreground group. The
    // server must not be in that group, or the launcher's forward would be a
    // second delivery that ends the drain early.
    process.kill(-run.child.pid, "SIGINT");
    const { code } = await run.exited;

    assert.equal(code, StatusSigint, run.stderr);
    assert.equal(countEvents(run.stdout, "child_sigint"), 1, run.stdout);
    assert.match(run.stdout, /"server_shutdown_completed"/);
    assert.equal(countEvents(run.stderr, "accounts_api_dev_signal"), 1);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "a second process-group SIGINT during the drain still ends the server",
  { skip, timeout: Timeout },
  async (t) => {
    const run = startLauncher(t, {
      port: await freePort(),
      pathPrefix: fakeGoBuildingCountingServer(t),
    });
    await waitReady(run);

    const started = Date.now();
    process.kill(-run.child.pid, "SIGINT");
    await new Promise((resolve) => setTimeout(resolve, 100));
    process.kill(-run.child.pid, "SIGINT");
    const { code } = await run.exited;

    assert.equal(code, StatusSigint, run.stderr);
    assert.equal(countEvents(run.stdout, "child_sigint"), 2, run.stdout);
    assert.doesNotMatch(run.stdout, /"server_shutdown_completed"/);
    assert.ok(
      Date.now() - started < DrainMs,
      "the second SIGINT did not end the drain early",
    );
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "SIGTERM to the launcher shuts the server down gracefully",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, { port });
    await waitReady(run);
    const health = await fetch(`http://127.0.0.1:${port}/healthz`);
    assert.equal(health.status, 200);
    assert.deepEqual(await health.json(), { status: "ok" });

    // Only the launcher receives the signal, as `kill <pid>` would deliver it.
    process.kill(run.child.pid, "SIGTERM");
    const { code } = await run.exited;

    assert.equal(code, StatusSigterm, run.stderr);
    assert.match(run.stdout, /"server_shutdown_started"/);
    assert.match(run.stdout, /"server_shutdown_completed"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "a process-group SIGINT stops the launcher and the server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, { port });
    await waitReady(run);

    // A terminal Ctrl-C reaches every process of the foreground group.
    process.kill(-run.child.pid, "SIGINT");
    const { code } = await run.exited;

    assert.equal(code, StatusSigint, run.stderr);
    assert.match(run.stdout, /"server_shutdown_completed"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "a process-group SIGQUIT reaches the server once and leaves no orphan",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, { port });
    await waitReady(run);
    const pids = [...run.childPids];
    assert.ok(pids.length > 0, "the server pid was not recorded");

    // A terminal Ctrl-\ signals every process of the foreground group. Only the
    // launcher is in that group; it must forward the signal to the server
    // instead of dying and orphaning the server.
    process.kill(-run.child.pid, "SIGQUIT");
    const { code } = await run.exited;

    assert.equal(code, StatusSigquit, run.stderr);
    assert.equal(countEvents(run.stderr, "accounts_api_dev_signal"), 1);
    assert.match(run.stderr, /"signal":"SIGQUIT"/);
    // The Go runtime prints one goroutine dump for the one SIGQUIT it receives.
    assert.equal(
      (`${run.stdout}\n${run.stderr}`.match(/SIGQUIT: quit/g) ?? []).length,
      1,
      `stdout: ${run.stdout}\nstderr: ${run.stderr}`,
    );
    // The Go runtime exits at once, so no graceful shutdown completes.
    assert.doesNotMatch(run.stdout, /"server_shutdown_completed"/);
    await waitGone(run, pids, "the server to exit");
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "an occupied port fails without a server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const holder = await occupy(port);
    t.after(() => holder.close());
    const run = startLauncher(t, { port });
    const { code } = await run.exited;

    assert.equal(code, 1, run.stderr);
    assert.match(run.stdout, /"server_exited"/);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "an invalid LOG_LEVEL fails without a server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, { port, env: { LOG_LEVEL: "bogus" } });
    const { code } = await run.exited;

    assert.equal(code, 1, run.stderr);
    assert.match(run.stdout, /"server_exited"/);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "a build failure exits with the build status and starts no server",
  { skip, timeout: Timeout },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, {
      port,
      pathPrefix: fakeGo(t, "exit 3"),
    });
    const { code } = await run.exited;

    assert.equal(code, 3, run.stderr);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "SIGTERM during the build never starts the server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const markerDir = mkdtempSync(join(tmpdir(), "accounts-api-marker-"));
    t.after(() => rmSync(markerDir, { recursive: true, force: true }));
    const marker = join(markerDir, "go-started");
    // The fake go waits, then on SIGTERM completes a real build and exits 0.
    // A launcher that ignored the termination would start the server next.
    const realGo = join(
      execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim(),
      "bin",
      "go",
    );
    const run = startLauncher(t, {
      port,
      env: { REAL_GO: realGo, MARKER: marker },
      pathPrefix: fakeGo(
        t,
        [
          'touch "$MARKER"',
          "sleep 60 &",
          "pid=$!",
          'trap \'kill $pid; exec "$REAL_GO" "$@"\' TERM',
          "wait $pid",
        ].join("\n"),
      ),
    });
    await waitFor(run, () => existsSync(marker), "the build to start");

    process.kill(run.child.pid, "SIGTERM");
    const { code } = await run.exited;

    assert.equal(code, StatusSigterm, run.stderr);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

test(
  "a process-group SIGQUIT during the build never starts the server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const markerDir = mkdtempSync(join(tmpdir(), "accounts-api-marker-"));
    t.after(() => rmSync(markerDir, { recursive: true, force: true }));
    const marker = join(markerDir, "go-started");
    // The fake go waits, then on SIGQUIT completes a real build and exits 0.
    // A launcher that ignored the signal would start the server next.
    const realGo = join(
      execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim(),
      "bin",
      "go",
    );
    const run = startLauncher(t, {
      port,
      env: { REAL_GO: realGo, MARKER: marker },
      pathPrefix: fakeGo(
        t,
        [
          'touch "$MARKER"',
          "sleep 60 &",
          "pid=$!",
          'trap \'kill $pid; exec "$REAL_GO" "$@"\' QUIT',
          "wait $pid",
        ].join("\n"),
      ),
    });
    await waitFor(run, () => existsSync(marker), "the build to start");

    process.kill(-run.child.pid, "SIGQUIT");
    const { code } = await run.exited;

    assert.equal(code, StatusSigquit, run.stderr);
    assert.equal(countEvents(run.stderr, "accounts_api_dev_signal"), 1);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

// Polls until none of the processes runs.
const waitGone = (run, pids, what) =>
  waitFor(run, () => pids.every((pid) => !isRunning(pid)), what);

test(
  "the test cleanup kills the running server and frees its port",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, { port });
    await waitReady(run);
    const pids = [...run.childPids];
    assert.ok(pids.length > 0, "the server pid was not recorded");
    assert.ok(pids.every(isRunning), "the recorded server is not running");
    assert.equal(await portInUse(port), true, "the server is not listening");

    stopLauncher(run);

    await run.exited;
    await waitGone(run, pids, "the server to be killed");
    await assertNoListener(port);
    assert.equal(existsSync(run.tmp), false, "the test TMPDIR was not removed");
  },
);

// The window between the launcher's module-level signal handlers and the start
// of the build is a few hundred microseconds wide (one `mkdtemp`), so a test
// cannot hit it by timing. This preload module widens it: it delays `mkdtemp`
// and announces the delayed call on standard error, which proves the launcher
// has installed its signal handlers (they are installed before `main` runs).
const MkdtempDelayMs = 1500;
const mkdtempDelayPreload = `
import fs from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
const mkdtemp = fs.mkdtemp;
fs.mkdtemp = async (...args) => {
  console.error(JSON.stringify({ event: "test_mkdtemp_delayed" }));
  await new Promise((resolve) => setTimeout(resolve, ${MkdtempDelayMs}));
  return mkdtemp(...args);
};
// Propagate the replacement to the named ESM imports of node:fs/promises.
syncBuiltinESMExports();
`;

test(
  "SIGTERM before the build starts never runs the build or the server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const supportDir = mkdtempSync(join(tmpdir(), "accounts-api-preload-"));
    t.after(() => rmSync(supportDir, { recursive: true, force: true }));
    const preload = join(supportDir, "delay-mkdtemp.mjs");
    writeFileSync(preload, mkdtempDelayPreload);
    const marker = join(supportDir, "go-started");
    // Any invocation of go leaves the marker. It exits non-zero at once so a
    // launcher that wrongly starts the build does not hang the test.
    const run = startLauncher(t, {
      port,
      env: {
        MARKER: marker,
        NODE_OPTIONS: `--import=${pathToFileURL(preload).href}`,
      },
      pathPrefix: fakeGo(t, ['touch "$MARKER"', "exit 1"].join("\n")),
    });
    await waitFor(
      run,
      () => countEvents(run.stderr, "test_mkdtemp_delayed") > 0,
      "the delayed mkdtemp call",
    );

    // The launcher is waiting for mkdtemp, so there is no child to forward the
    // signal to; the launcher has to act on it before it starts the build.
    process.kill(run.child.pid, "SIGTERM");
    const { code } = await run.exited;

    assert.equal(code, StatusSigterm, run.stderr);
    assert.equal(existsSync(marker), false, "go was started after the signal");
    assert.doesNotMatch(run.stdout, /"server_started"/);
    assert.equal(countEvents(run.stderr, "accounts_api_dev_signal"), 1);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);
