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
import { fileURLToPath } from "node:url";
import test from "node:test";

// These tests start the real launcher, which builds and runs the real server.
// They use POSIX signals and process groups, so Windows is skipped like the
// formatter signal tests.
const skip = process.platform === "win32";
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const StatusSigint = 130;
const StatusSigterm = 143;
const Timeout = 60_000;

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

// Remembers the launcher's current children (the build or the server). Call it
// while the launcher is alive: once the launcher dies, its children are
// reparented and `pgrep -P <launcher pid>` no longer finds them.
function recordChildren(run) {
  for (const pid of childPids(run.child.pid)) run.childPids.add(pid);
}

// Kills everything a launcher test may have left behind. The launcher runs each
// child (the build and the server) in its own process group (see dev.mjs), so
// signalling the launcher's group never reaches them; they are killed by pid
// instead. Recorded pids cover children that outlived a launcher that died
// first; a fresh sweep covers a child started since the last record. Children
// go first, while the launcher is still alive, so none is orphaned. The pids are
// recorded moments earlier in the same test, so a pid reused by an unrelated
// process is not a practical risk.
function stopLauncher(run) {
  recordChildren(run);
  for (const pid of run.childPids) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // The process is already gone.
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
// cleanup (`stopLauncher`) kills them by pid. The temporary build output goes
// to a test-owned TMPDIR.
function startLauncher(t, { port, env = {}, pathPrefix } = {}) {
  const tmp = mkdtempSync(join(tmpdir(), "accounts-api-dev-test-"));
  const childEnv = { ...process.env, ADDR: `127.0.0.1:${port}`, TMPDIR: tmp };
  delete childEnv.LOG_LEVEL;
  Object.assign(childEnv, env);
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
  const deadline = Date.now() + Timeout;
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

test(
  "the test cleanup kills a server orphaned by a dead launcher",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, { port });
    await waitReady(run);
    const pids = [...run.childPids];
    assert.ok(pids.length > 0, "the server pid was not recorded");

    // Killing the launcher's own process group does not reach the server, which
    // runs in a group of its own: it survives as an orphan and keeps the port.
    // `pgrep -P <launcher pid>` can no longer find it, so the pid recorded
    // while the launcher was alive is what cleanup has to use.
    // `run.exited` is not awaited: the orphan holds the launcher's stdio pipes
    // open, so the launcher's "close" event waits for the orphan to exit.
    process.kill(-run.child.pid, "SIGKILL");
    await waitGone(run, [run.child.pid], "the launcher to die");
    assert.deepEqual(childPids(run.child.pid), []);
    assert.ok(pids.every(isRunning), "the server did not outlive the launcher");
    assert.equal(await portInUse(port), true, "the orphan stopped listening");

    stopLauncher(run);

    await waitGone(run, pids, "the orphaned server to be killed");
    await assertNoListener(port);
  },
);
