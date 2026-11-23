// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

// Process lifecycle of the ownsand-api development launcher with a fake `go`
// and a fake server, so no Go toolchain, database or real build is involved.
// They cover the forced stop (SIGKILL of the task's process group) during the
// build and during the server phase, and the signal forwarding that the
// supervisor of each phase must keep: one delivery per signal. They use POSIX
// signals and process groups, so Windows is skipped.
const skip = process.platform === "win32";
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const SignalEvent = "ownsand_api_dev_signal";
const Timeout = 60_000;
// Stays below Timeout so that the diagnostic of waitFor is printed before the
// runner reports a bare timeout.
const WaitTimeout = 45_000;

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function freePort() {
  const probe = createServer();
  await new Promise((resolve) => probe.listen(0, "127.0.0.1", resolve));
  const { port } = probe.address();
  await new Promise((resolve) => probe.close(resolve));
  return port;
}

// Resolves true when binding the port fails, which means a listener holds it.
function portInUse(port) {
  return new Promise((resolve) => {
    const probe = createServer();
    probe.once("error", () => resolve(true));
    probe.listen(port, "127.0.0.1", () => probe.close(() => resolve(false)));
  });
}

// A killed process that has not been reaped yet is a zombie: it holds no port
// and is not running, so it counts as gone.
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

function parentOf(pid) {
  try {
    const out = execFileSync("ps", ["-o", "ppid=", "-p", String(pid)], {
      encoding: "utf8",
    }).trim();
    return out === "" ? null : Number(out);
  } catch {
    return null;
  }
}

// The fake server. Every forwarded signal prints one event. The mode (set
// through FAKE_MODE) selects the reaction:
//   drain       SIGINT and SIGTERM start a drain that ends after a short time
//               with a completion event; a second SIGINT ends it at once.
//   hold        like drain, but the drain never ends by itself, so only a
//               second signal can end the server; no timing is involved.
//   ignore-term SIGTERM is ignored; SIGINT and SIGQUIT end the server.
//   child       like drain, and the server runs a descendant that also lives
//               until it is killed and reports its pid.
// SIGQUIT ends the server at once like the Go runtime does.
const fakeServer = `#!${process.execPath}
// getBuiltinModule works whether the file is treated as CommonJS or as an ES
// module, which depends on the package.json above the temporary directory.
const net = process.getBuiltinModule("node:net");
const { spawn } = process.getBuiltinModule("node:child_process");
const mode = process.env.FAKE_MODE || "drain";
const counts = { SIGINT: 0, SIGTERM: 0, SIGQUIT: 0 };
const log = (event) => console.log(JSON.stringify(event));
for (const signal of Object.keys(counts)) {
  process.on(signal, () => {
    counts[signal] += 1;
    log({ event: "fake_signal", signal, n: counts[signal] });
    if (signal === "SIGQUIT") process.exit(2);
    if (mode === "ignore-term" && signal === "SIGTERM") return;
    if (mode === "ignore-term") process.exit(2);
    if (counts.SIGINT + counts.SIGTERM > 1) process.exit(2);
    if (mode === "hold") return;
    setTimeout(() => {
      log({ event: "fake_shutdown_completed" });
      process.exit(0);
    }, 200);
  });
}
let descendant = null;
if (mode === "child") {
  descendant = spawn("sleep", ["300"], { stdio: "ignore" });
  descendant.unref();
}
net
  .createServer()
  .listen(Number(process.env.FAKE_SERVER_PORT), "127.0.0.1", () => {
    log({ event: "fake_server", pid: process.pid, descendant: descendant && descendant.pid });
  });
`;

// The fake go: records its pid, then either builds the fake server (mode
// "server") or stays in a build that runs a descendant (mode "build"). With
// FAKE_STUBBORN the build ignores SIGTERM, as does its descendant, so only
// SIGKILL ends it.
function fakeGo(t) {
  const dir = mkdtempSync(join(tmpdir(), "ownsand-api-fake-go-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "go");
  writeFileSync(
    file,
    `#!/bin/sh
if [ "$FAKE_PHASE" = fd ]; then
  # Lists the open descriptors of the build; sh opens none of its own.
  for fd in /proc/$$/fd/*; do echo "$(basename "$fd") $(readlink "$fd")"; done > "$MARKER_DIR/fds" 2>/dev/null
  echo done >> "$MARKER_DIR/fds"
  exit 0
fi
if [ "$FAKE_PHASE" = build ]; then
  [ -n "$FAKE_STUBBORN" ] && trap '' TERM
  echo "$$" > "$MARKER_DIR/go.pid"
  sleep 300 &
  echo "$!" > "$MARKER_DIR/descendant.pid"
  wait
  exit 0
fi
while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done
cat > "$out" <<'PROGRAM'
${fakeServer}PROGRAM
chmod 755 "$out"
`,
  );
  chmodSync(file, 0o755);
  return dir;
}

// Starts the launcher as the leader of a process group of its own, as a task
// runner does, so a test can signal the group.
async function startLauncher(t, { env = {} } = {}) {
  const tmp = mkdtempSync(join(tmpdir(), "ownsand-api-forced-stop-"));
  const markerDir = join(tmp, "markers");
  const buildTmp = join(tmp, "tmp");
  mkdirSync(markerDir);
  mkdirSync(buildTmp);
  const port = await freePort();
  const childEnv = {
    ...process.env,
    DATABASE_URL: "postgres://placeholder@127.0.0.1:1/placeholder",
    FAKE_SERVER_PORT: String(port),
    MARKER_DIR: markerDir,
    PATH: `${fakeGo(t)}${delimiter}${process.env.PATH}`,
    TMPDIR: buildTmp,
    ...env,
  };
  delete childEnv.LOG_LEVEL;
  const child = spawn(process.execPath, [launcher], {
    env: childEnv,
    stdio: ["ignore", "pipe", "pipe"],
    detached: true,
  });
  const run = {
    child,
    port,
    markerDir,
    buildTmp,
    stdout: "",
    stderr: "",
    pids: new Set(),
    exit: null,
  };
  child.stdout.on("data", (chunk) => (run.stdout += chunk));
  child.stderr.on("data", (chunk) => (run.stderr += chunk));
  // "exit", not "close": a survivor that holds the output pipes open would
  // delay "close" until it ends.
  run.exited = new Promise((resolve) =>
    child.once("exit", (code, signal) => {
      run.exit = { code, signal };
      resolve(run.exit);
    }),
  );
  t.after(() => {
    stopAll(run);
    rmSync(tmp, { recursive: true, force: true });
  });
  return run;
}

// Kills everything that a test recorded (the supervisor leads the group of its
// child, so its group kill ends both), then the launcher's group. Cleanup after
// a failed test must leave no process or listening port behind. Only pids that
// the test recorded while their processes were alive are used, never a parent
// looked up now: the parent of an orphan is init.
function stopAll(run) {
  for (const pid of run.pids) {
    for (const target of [-pid, pid]) {
      if (!(pid > 1)) continue;
      try {
        process.kill(target, "SIGKILL");
      } catch {
        // Already gone.
      }
    }
  }
  try {
    process.kill(-run.child.pid, "SIGKILL");
  } catch {
    // The launcher's group is already gone.
  }
}

async function waitFor(run, predicate, what) {
  const deadline = Date.now() + WaitTimeout;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await pause(25);
  }
  assert.fail(
    `timed out waiting for ${what}\nstdout: ${run.stdout}\nstderr: ${run.stderr}`,
  );
}

const events = (output, event) =>
  output
    .split("\n")
    .filter((line) => line.includes(`"event":"${event}"`))
    .map((line) => JSON.parse(line));

// Waits for the fake server and records its pid, its supervisor and the pid of
// its descendant, while the launcher is alive.
async function waitServer(run) {
  await waitFor(
    run,
    () => events(run.stdout, "fake_server").length > 0,
    "the server",
  );
  const [server] = events(run.stdout, "fake_server");
  run.pids.add(server.pid);
  if (server.descendant) run.pids.add(server.descendant);
  run.supervisor = parentOf(server.pid);
  run.pids.add(run.supervisor);
  return server;
}

const readPid = (file) => Number(readFileSync(file, "utf8").trim());

// Waits for the fake build and records its pid, its descendant and its
// supervisor.
async function waitBuild(run) {
  const go = join(run.markerDir, "go.pid");
  const descendant = join(run.markerDir, "descendant.pid");
  await waitFor(
    run,
    () => existsSync(go) && existsSync(descendant),
    "the build",
  );
  const pids = { go: readPid(go), descendant: readPid(descendant) };
  for (const pid of Object.values(pids)) run.pids.add(pid);
  run.supervisor = parentOf(pids.go);
  run.pids.add(run.supervisor);
  return pids;
}

const waitGone = (run, pids, what) =>
  waitFor(run, () => pids.every((pid) => !isRunning(pid)), what);

// The supervisor is the launcher's child and leads the group of the phase.
function assertSupervised(run, server) {
  assert.equal(parentOf(run.supervisor), run.child.pid, "supervisor parent");
  assert.notEqual(run.supervisor, server);
}

for (const [phase, label] of [
  ["server", "the server phase"],
  ["build", "the build phase"],
]) {
  test(
    `SIGKILL of the launcher's process group ends ${label} and its descendants`,
    { skip, timeout: Timeout },
    async (t) => {
      const run = await startLauncher(t, {
        env: { FAKE_PHASE: phase, FAKE_MODE: "child" },
      });
      let all;
      if (phase === "server") {
        const server = await waitServer(run);
        assert.equal(
          await portInUse(run.port),
          true,
          "the server is not listening",
        );
        assertSupervised(run, server.pid);
        all = [server.pid, server.descendant, run.supervisor];
      } else {
        const build = await waitBuild(run);
        assert.equal(parentOf(run.supervisor), run.child.pid);
        all = [build.go, build.descendant, run.supervisor];
      }
      assert.ok(all.every(isRunning), "a recorded process is not running");

      // What a task runner's forced stop does: SIGKILL to the whole group of
      // the task. The launcher is in it; the supervised phase is not.
      process.kill(-run.child.pid, "SIGKILL");
      const { signal } = await run.exited;
      assert.equal(signal, "SIGKILL");

      await waitGone(run, all, "the phase and its descendants to end");
      assert.equal(await portInUse(run.port), false, "a listener survived");
      if (phase === "build") {
        assert.equal(
          events(run.stdout, "fake_server").length,
          0,
          "server started",
        );
      }
    },
  );
}

for (const [phase, label] of [
  ["server", "a server that ignores SIGTERM"],
  ["build", "a build that ignores SIGTERM"],
]) {
  test(
    `a forced stop ends ${label} with SIGKILL after the grace period`,
    { skip, timeout: Timeout },
    async (t) => {
      const run = await startLauncher(t, {
        env:
          phase === "server"
            ? { FAKE_MODE: "ignore-term" }
            : { FAKE_PHASE: "build", FAKE_STUBBORN: "1" },
      });
      let all;
      if (phase === "server") {
        const server = await waitServer(run);
        all = [server.pid, run.supervisor];
      } else {
        const build = await waitBuild(run);
        all = [build.go, build.descendant, run.supervisor];
      }

      process.kill(-run.child.pid, "SIGKILL");
      await run.exited;

      await waitGone(run, all, "the phase to be killed");
      assert.equal(await portInUse(run.port), false, "a listener survived");
    },
  );
}

test(
  "SIGHUP to the launcher ends the server as well",
  { skip, timeout: Timeout },
  async (t) => {
    const run = await startLauncher(t);
    const server = await waitServer(run);

    // Closing the terminal ends the launcher, which does not handle SIGHUP.
    process.kill(run.child.pid, "SIGHUP");
    const { signal } = await run.exited;
    assert.equal(signal, "SIGHUP");

    await waitGone(run, [server.pid, run.supervisor], "the server to end");
    assert.equal(await portInUse(run.port), false, "a listener survived");
  },
);

test(
  "the launcher's pipe does not reach the build",
  { skip: skip || !existsSync("/proc/self/fd"), timeout: Timeout },
  async (t) => {
    const run = await startLauncher(t, { env: { FAKE_PHASE: "fd" } });
    await run.exited;

    // The supervisor's descriptor 3 is the pipe to the launcher. Its child
    // gets /dev/null there, so the pipe does not reach the build.
    const fds = readFileSync(join(run.markerDir, "fds"), "utf8");
    assert.match(fds, /done\n$/);
    assert.match(fds, /^3 \/dev\/null$/m);
  },
);

test(
  "a group SIGINT reaches the server once and the drain completes",
  { skip, timeout: Timeout },
  async (t) => {
    const run = await startLauncher(t);
    await waitServer(run);

    process.kill(-run.child.pid, "SIGINT");
    const { code } = await run.exited;

    assert.equal(code, 130, run.stderr);
    assert.deepEqual(
      events(run.stdout, "fake_signal").map(({ signal, n }) => [signal, n]),
      [["SIGINT", 1]],
    );
    assert.equal(events(run.stdout, "fake_shutdown_completed").length, 1);
    assert.equal(events(run.stderr, SignalEvent).length, 1);
    assert.deepEqual(
      readdirSync(run.buildTmp),
      [],
      "build output was not removed",
    );
    assert.equal(await portInUse(run.port), false);
  },
);

test(
  "a second group SIGINT during the drain ends the server",
  { skip, timeout: Timeout },
  async (t) => {
    const run = await startLauncher(t, { env: { FAKE_MODE: "hold" } });
    await waitServer(run);

    process.kill(-run.child.pid, "SIGINT");
    await waitFor(
      run,
      () => events(run.stdout, "fake_signal").length === 1,
      "the drain",
    );
    // The drain never ends by itself in this mode: the launcher is still
    // running until the second signal arrives.
    assert.equal(run.exit, null);
    process.kill(-run.child.pid, "SIGINT");
    const { code } = await run.exited;

    assert.equal(code, 130, run.stderr);
    assert.deepEqual(
      events(run.stdout, "fake_signal").map(({ signal, n }) => [signal, n]),
      [
        ["SIGINT", 1],
        ["SIGINT", 2],
      ],
    );
    assert.equal(events(run.stdout, "fake_shutdown_completed").length, 0);
    assert.deepEqual(
      readdirSync(run.buildTmp),
      [],
      "build output was not removed",
    );
  },
);

test(
  "a direct SIGTERM to the launcher reaches the server once",
  { skip, timeout: Timeout },
  async (t) => {
    const run = await startLauncher(t);
    await waitServer(run);

    process.kill(run.child.pid, "SIGTERM");
    const { code } = await run.exited;

    assert.equal(code, 143, run.stderr);
    assert.deepEqual(
      events(run.stdout, "fake_signal").map(({ signal, n }) => [signal, n]),
      [["SIGTERM", 1]],
    );
    assert.equal(events(run.stdout, "fake_shutdown_completed").length, 1);
    assert.deepEqual(
      readdirSync(run.buildTmp),
      [],
      "build output was not removed",
    );
    assert.equal(await portInUse(run.port), false);
  },
);

test(
  "a group SIGQUIT reaches the server once and the launcher exits 131",
  { skip, timeout: Timeout },
  async (t) => {
    const run = await startLauncher(t);
    const server = await waitServer(run);

    process.kill(-run.child.pid, "SIGQUIT");
    const { code } = await run.exited;

    assert.equal(code, 131, run.stderr);
    assert.deepEqual(
      events(run.stdout, "fake_signal").map(({ signal, n }) => [signal, n]),
      [["SIGQUIT", 1]],
    );
    assert.equal(events(run.stdout, "fake_shutdown_completed").length, 0);
    assert.equal(events(run.stderr, SignalEvent).length, 1);
    await waitGone(run, [server.pid, run.supervisor], "the server to end");
    assert.deepEqual(
      readdirSync(run.buildTmp),
      [],
      "build output was not removed",
    );
  },
);
