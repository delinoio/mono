// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import {
  existsSync,
  fstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { createInterface } from "node:readline";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import test from "node:test";

const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const fixturePreload = new URL(
  "./dev-forced-stop-preload.mjs",
  import.meta.url,
);
const fixture = fileURLToPath(import.meta.url);
const signals = ["SIGINT", "SIGTERM", "SIGQUIT"];
const options = { skip: process.platform === "win32", timeout: 60_000 };
const stopObservationTimeout = 4_000;
const fakeServer =
  '#!/bin/sh\nexec "$FIXTURE_NODE" "$FIXTURE_SCRIPT" --fixture server\n';

function report(event) {
  writeFileSync(1, JSON.stringify(event) + "\n");
}

// Run the fixture under the real launcher as either the fake Go build or its
// output binary. The descendant records signals but stays alive so a
// successful exit of its parent leaves a process for the supervisor to stop.
async function runFixture(phase, args) {
  const deadline = performance.now() + options.timeout;
  const checkStop = () => {
    // A canceled build must fail so the launcher does not start the server.
    if (
      existsSync(process.env.FIXTURE_STOP_FILE) ||
      performance.now() >= deadline
    ) {
      process.exit(1);
    }
  };
  checkStop();

  const buildServer = () =>
    writeFileSync(args[args.indexOf("-o") + 1], fakeServer, { mode: 0o755 });
  if (phase === "build" && process.env.FIXTURE_PHASE === "server") {
    buildServer();
    return;
  }

  // Poll while starting up as well as while waiting for the test's signals.
  setInterval(checkStop, 20);
  if (phase === "descendant") {
    for (const signal of signals) {
      process.on(signal, () => report({ event: "descendant_signal", signal }));
    }
    process.send("ready");
    setInterval(() => {}, 1000);
    return;
  }

  let canExit = false;
  for (const signal of signals) {
    process.on(signal, () => {
      report({ event: "signal", signal });
      if (signal === "SIGTERM" && process.env.FIXTURE_STOP === "ignore-term") {
        return;
      }
      canExit = true;
    });
  }

  const descendant = spawn(
    process.execPath,
    [fixture, "--fixture", "descendant"],
    { stdio: ["ignore", "inherit", "inherit", "ipc"] },
  );
  await once(descendant, "message");

  const ports = [];
  if (phase === "server") {
    for (let i = 0; i < 2; i += 1) {
      const listener = createServer().listen(0, "127.0.0.1");
      await once(listener, "listening");
      ports.push(listener.address().port);
    }
  }

  let hasNullFd = false;
  try {
    const fd = fstatSync(3);
    const devNull = statSync("/dev/null");
    hasNullFd = fd.dev === devNull.dev && fd.ino === devNull.ino;
  } catch {
    // Report the actual descriptor state, including on the pre-fix launcher.
  }
  report({
    event: "ready",
    phase,
    pid: process.pid,
    descendant: descendant.pid,
    ports,
    hasNullFd,
  });

  setInterval(() => {
    if (!canExit) return;
    if (
      process.env.FIXTURE_STOP === "wait" &&
      !existsSync(process.env.FIXTURE_EXIT)
    ) {
      return;
    }
    if (phase === "build") buildServer();
    report({ event: "completed" });
    process.exit(0);
  }, 20);
}

function ps(field, pid) {
  try {
    return execFileSync("ps", ["-o", field + "=", "-p", String(pid)], {
      encoding: "utf8",
    }).trim();
  } catch (error) {
    if (error.status !== 1) throw error;
    return "";
  }
}

function isRunning(pid) {
  const state = ps("stat", pid);
  // Orphaned zombies on some hosts have exited and hold no resources.
  return state !== "" && !state.startsWith("Z");
}

function startLauncher(t, phase, stop, holdPhaseBoundary = false) {
  const dir = mkdtempSync(join(tmpdir(), "sandrun-api-forced-stop-"));
  const buildTmp = join(dir, "builds");
  const exitFile = join(dir, "exit");
  const releaseGraceFile = join(dir, "release-grace");
  const releasePhaseFile = join(dir, "release-phase");
  const stopFile = join(dir, "stop");
  let run;
  t.after(async () => {
    try {
      if (run && !run.outputClosed) {
        writeFileSync(stopFile, "");
        writeFileSync(releaseGraceFile, "");
        writeFileSync(releasePhaseFile, "");
        await waitFor(
          run,
          () => run.outputClosed,
          "the launcher to exit and its output pipes to close",
          10_000,
        );
      }
    } finally {
      if (run && !run.outputClosed) {
        // A cleanup failure must not leave the test runner waiting on handles.
        run.task.stdout.destroy();
        run.task.stderr.destroy();
        run.task.unref();
      }
      rmSync(dir, { recursive: true, force: true });
    }
  });
  mkdirSync(buildTmp);
  writeFileSync(
    join(dir, "go"),
    '#!/bin/sh\nexec "$FIXTURE_NODE" "$FIXTURE_SCRIPT" --fixture build "$@"\n',
    { mode: 0o755 },
  );
  const env = {
    ...process.env,
    DATABASE_URL: "postgres://launcher-test",
    PATH: dir + delimiter + process.env.PATH,
    TMPDIR: buildTmp,
    FIXTURE_SCRIPT: fixture,
    FIXTURE_NODE: process.execPath,
    FIXTURE_PHASE: phase,
    FIXTURE_STOP: stop,
    FIXTURE_EXIT: exitFile,
    FIXTURE_STOP_FILE: stopFile,
  };
  if (phase === "server" && stop === "ignore-term") {
    env.FIXTURE_RELEASE_GRACE = releaseGraceFile;
  }
  if (holdPhaseBoundary) env.FIXTURE_RELEASE_PHASE = releasePhaseFile;
  if (env.FIXTURE_RELEASE_GRACE || holdPhaseBoundary) {
    env.NODE_OPTIONS = [env.NODE_OPTIONS, `--import=${fixturePreload}`]
      .filter(Boolean)
      .join(" ");
  }
  const task = spawn(process.execPath, [launcher], {
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
    env,
  });
  run = {
    task,
    buildTmp,
    exitFile,
    releaseGraceFile,
    records: [],
    stderr: "",
    exit: null,
    outputClosed: false,
  };
  createInterface({ input: task.stdout }).on("line", (line) => {
    run.records.push(JSON.parse(line));
  });
  task.stderr.setEncoding("utf8").on("data", (chunk) => (run.stderr += chunk));
  // "close" can wait for pipes that a surviving phase inherited.
  run.exited = once(task, "exit").then(([code, signal]) => {
    run.exit = { code, signal };
    return run.exit;
  });
  run.exited.catch((error) => {
    run.stderr += String(error) + "\n";
  });
  run.closed = new Promise((resolve) => {
    task.once("close", () => {
      run.outputClosed = true;
      resolve();
    });
  });
  return run;
}

async function waitFor(run, condition, description, timeout = 15_000) {
  const deadline = Date.now() + timeout;
  do {
    const result = condition();
    if (result) return result;
    await delay(25);
  } while (Date.now() < deadline);
  assert.fail(
    `Waited for ${description}\n${JSON.stringify(run.records)}\n${run.stderr}`,
  );
}

const ready = (run) =>
  waitFor(
    run,
    () => run.records.find(({ event }) => event === "ready"),
    "phase readiness",
  );

const observedSignals = (run, source = "signal") =>
  run.records
    .filter(({ event }) => event === source)
    .map(({ signal }) => signal);

function buildDirectory(run) {
  const entries = readdirSync(run.buildTmp);
  assert.equal(entries.length, 1);
  assert.ok(entries[0].startsWith("sandrun-api-dev-"));
  return join(run.buildTmp, entries[0]);
}

const waitForBuildCleanup = (run, buildDir) =>
  waitFor(
    run,
    () => !existsSync(buildDir),
    "the launch's build directory to be removed",
    stopObservationTimeout,
  );

async function bindLoopbackPort(port) {
  const signal = AbortSignal.timeout(stopObservationTimeout);
  const probe = createServer().listen({ host: "127.0.0.1", port, signal });
  await once(probe, "listening", { signal });
  const closed = once(probe, "close", { signal });
  probe.close();
  await closed;
}

async function assertPortsReleased(ports) {
  for (const port of ports) {
    await assert.doesNotReject(
      bindLoopbackPort(port),
      `rebind loopback port ${port}`,
    );
  }
}

if (process.argv[2] === "--fixture") {
  await runFixture(process.argv[3], process.argv.slice(4));
} else {
  test(
    "teardown stops a waiting server and its descendant",
    options,
    async (t) => {
      const run = startLauncher(t, "server", "wait");
      const active = await ready(run);
      assert.ok(isRunning(active.pid) && isRunning(active.descendant));
      assert.equal(run.outputClosed, false);

      t.after(async () => {
        assert.deepEqual(run.exit, { code: 1, signal: null });
        assert.equal(run.outputClosed, true);
        assert.ok(!isRunning(active.pid) && !isRunning(active.descendant));
        await assertPortsReleased(active.ports);
      });
    },
  );

  for (const [phase, stop] of [
    ["build", "exit"],
    ["server", "exit"],
    ["server", "ignore-term"],
  ]) {
    test(
      `a task-group SIGKILL stops the ${phase} and descendants (${stop})`,
      options,
      async (t) => {
        const run = startLauncher(t, phase, stop);
        const active = await ready(run);
        assert.equal(active.phase, phase);
        assert.ok(active.descendant > 1);
        assert.ok(isRunning(active.pid) && isRunning(active.descendant));
        const buildDir = buildDirectory(run);
        if (phase === "server") {
          assert.equal(active.ports.length, 2);
          assert.equal(
            readFileSync(join(buildDir, "sandrun-api"), "utf8"),
            fakeServer,
          );
        }

        process.kill(-run.task.pid, "SIGKILL");
        const exit = await waitFor(
          run,
          () => run.exit,
          "the launcher to exit after task-group SIGKILL",
          stopObservationTimeout,
        );
        assert.deepEqual(exit, { code: null, signal: "SIGKILL" });
        const pids = [active.pid, active.descendant];
        if (stop === "ignore-term") {
          const grace = await waitFor(
            run,
            () => run.records.find(({ event }) => event === "grace_timer"),
            "the server supervisor to schedule escalation",
            stopObservationTimeout,
          );
          assert.equal(grace.delay, 5_000, "the escalation delay in ms");
          await waitFor(
            run,
            () => observedSignals(run).length,
            "the server to receive its first shutdown signal",
            stopObservationTimeout,
          );
          assert.deepEqual(observedSignals(run), ["SIGTERM"]);
          assert.ok(isRunning(active.pid), "the server stopped before release");
          assert.ok(
            isRunning(active.descendant),
            "the descendant stopped before release",
          );
          assert.ok(
            existsSync(buildDir),
            "the build directory was removed before shutdown",
          );
          for (const port of active.ports) {
            await assert.rejects(
              bindLoopbackPort(port),
              { code: "EADDRINUSE" },
              `loopback port ${port} must remain bound before release`,
            );
          }
          pids.push(grace.pid);
          writeFileSync(run.releaseGraceFile, "");
        }
        await waitFor(
          run,
          () => pids.every((pid) => !isRunning(pid)),
          stop === "ignore-term"
            ? "the server supervisor, server and descendant to stop after releasing escalation"
            : "the phase and its descendant to stop",
          stopObservationTimeout,
        );
        await waitFor(
          run,
          () => run.outputClosed,
          "the launcher output pipes to close after the group stops",
          stopObservationTimeout,
        );
        await waitForBuildCleanup(run, buildDir);

        assert.equal(
          active.hasNullFd,
          true,
          "the lifetime pipe must not reach the phase",
        );
        assert.deepEqual(observedSignals(run), ["SIGTERM"]);
        assert.equal(
          run.records.filter(({ event }) => event === "ready").length,
          1,
        );
        if (phase === "server") await assertPortsReleased(active.ports);
      },
    );
  }

  test(
    "a task-group SIGKILL between phases removes only its own build directory",
    options,
    async (t) => {
      const run = startLauncher(t, "server", "exit", true);
      await waitFor(
        run,
        () => run.records.some(({ event }) => event === "phase_boundary"),
        "the build to finish before the server phase starts",
      );
      const buildDir = buildDirectory(run);
      assert.equal(
        readFileSync(join(buildDir, "sandrun-api"), "utf8"),
        fakeServer,
      );
      const sibling = join(run.buildTmp, "sandrun-api-dev-sibling");
      mkdirSync(sibling);
      const sentinel = join(sibling, "sentinel");
      writeFileSync(sentinel, "keep");

      process.kill(-run.task.pid, "SIGKILL");
      await waitForBuildCleanup(run, buildDir);
      await waitFor(run, () => run.outputClosed, "the output pipes to close");
      assert.deepEqual(run.exit, { code: null, signal: "SIGKILL" });
      assert.ok(!run.records.some(({ event }) => event === "ready"));
      assert.equal(readFileSync(sentinel, "utf8"), "keep");
    },
  );

  for (const [requests, code] of [
    [["SIGINT"], 130],
    [["SIGTERM"], 143],
    [["SIGQUIT", "SIGINT"], 131],
  ]) {
    test(
      `${requests.join(" then ")} reaches the server once per request and stops its descendants after drain (status ${code})`,
      options,
      async (t) => {
        const run = startLauncher(t, "server", "wait");
        const active = await ready(run);
        assert.equal(active.phase, "server");
        assert.equal(active.ports.length, 2);
        assert.ok(active.descendant > 1);
        assert.equal(ps("pgid", active.pid), ps("pgid", active.descendant));

        for (const [index, signal] of requests.entries()) {
          process.kill(
            signal === "SIGTERM" ? run.task.pid : -run.task.pid,
            signal,
          );
          await waitFor(
            run,
            () => observedSignals(run).length > index,
            `the server to receive ${signal}`,
          );
        }
        assert.equal(run.exit, null, "the launcher exited before its phase");
        assert.ok(isRunning(active.pid) && isRunning(active.descendant));
        assert.equal(readdirSync(run.buildTmp).length, 1);
        writeFileSync(run.exitFile, "");

        await waitFor(
          run,
          () => !isRunning(active.pid) && !isRunning(active.descendant),
          "the server and its descendant to stop",
          4_000,
        );
        assert.deepEqual(await run.exited, { code, signal: null });
        await run.closed;
        assert.deepEqual(observedSignals(run), requests);
        assert.deepEqual(observedSignals(run, "descendant_signal"), []);
        assert.equal(
          run.records.filter(({ event }) => event === "completed").length,
          1,
        );
        assert.equal(
          run.records.filter(({ event }) => event === "ready").length,
          1,
        );
        assert.deepEqual(readdirSync(run.buildTmp), []);
        await assertPortsReleased(active.ports);
      },
    );
  }

  for (const [requests, code] of [
    [["SIGINT"], 130],
    [["SIGTERM"], 143],
    [["SIGQUIT", "SIGINT"], 131],
  ]) {
    test(
      `${requests.join(" then ")} reaches the build group once per request (status ${code})`,
      options,
      async (t) => {
        const run = startLauncher(t, "build", "wait");
        const active = await ready(run);
        assert.equal(active.phase, "build");
        assert.equal(active.ports.length, 0);
        assert.ok(active.hasNullFd);

        for (const [index, signal] of requests.entries()) {
          process.kill(
            signal === "SIGTERM" ? run.task.pid : -run.task.pid,
            signal,
          );
          await waitFor(
            run,
            () =>
              observedSignals(run).length > index &&
              observedSignals(run, "descendant_signal").length > index,
            `the build group to receive ${signal}`,
            4_000,
          );
        }

        assert.equal(run.exit, null, "the launcher exited before the build");
        assert.ok(isRunning(active.pid) && isRunning(active.descendant));
        assert.equal(readdirSync(run.buildTmp).length, 1);
        writeFileSync(run.exitFile, "");

        await waitFor(
          run,
          () => !isRunning(active.pid) && !isRunning(active.descendant),
          "the build group to stop promptly",
          4_000,
        );
        assert.deepEqual(await run.exited, { code, signal: null });
        await run.closed;
        assert.deepEqual(observedSignals(run), requests);
        assert.deepEqual(observedSignals(run, "descendant_signal"), requests);
        assert.equal(
          run.records.filter(({ event }) => event === "completed").length,
          1,
        );
        assert.equal(
          run.records.filter(({ event }) => event === "ready").length,
          1,
        );
        assert.deepEqual(readdirSync(run.buildTmp), []);
      },
    );
  }

  test(
    "SIGTERM kills a resistant build group at the deadline",
    options,
    async (t) => {
      const run = startLauncher(t, "build", "ignore-term");
      const active = await ready(run);

      const started = performance.now();
      process.kill(run.task.pid, "SIGTERM");
      await waitFor(
        run,
        () =>
          observedSignals(run).length &&
          observedSignals(run, "descendant_signal").length,
        "the build group to receive SIGTERM",
      );
      assert.equal(run.exit, null);
      assert.ok(isRunning(active.pid) && isRunning(active.descendant));
      assert.equal(readdirSync(run.buildTmp).length, 1);
      await waitFor(
        run,
        () => !isRunning(active.pid) && !isRunning(active.descendant),
        "the build group to stop at the deadline",
        12_000,
      );
      assert.ok(performance.now() - started >= 4_900);

      assert.deepEqual(await run.exited, { code: 143, signal: null });
      await run.closed;
      assert.deepEqual(observedSignals(run), ["SIGTERM"]);
      assert.deepEqual(observedSignals(run, "descendant_signal"), ["SIGTERM"]);
      assert.equal(
        run.records.filter(({ event }) => event === "ready").length,
        1,
      );
      assert.equal(
        run.records.filter(({ event }) => event === "completed").length,
        0,
      );
      assert.deepEqual(readdirSync(run.buildTmp), []);
    },
  );
}
