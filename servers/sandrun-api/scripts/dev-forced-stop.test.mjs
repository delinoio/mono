// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import {
  existsSync,
  fstatSync,
  mkdirSync,
  mkdtempSync,
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
const fixture = fileURLToPath(import.meta.url);
const signals = ["SIGINT", "SIGTERM", "SIGQUIT"];
const options = { skip: process.platform === "win32", timeout: 60_000 };
const fakeServer =
  '#!/bin/sh\nexec "$FIXTURE_NODE" "$FIXTURE_SCRIPT" --fixture server\n';

function report(event) {
  writeFileSync(1, JSON.stringify(event) + "\n");
}

// Run the fixture under the real launcher as either the fake Go build or its
// output binary. The descendant records signals but stays alive so a
// successful exit of its parent leaves a process for the supervisor to stop.
async function runFixture(phase, args) {
  if (phase === "descendant") {
    for (const signal of signals) {
      process.on(signal, () => report({ event: "descendant_signal", signal }));
    }
    process.send("ready");
    setInterval(() => {}, 1000);
    return;
  }

  const buildServer = () =>
    writeFileSync(args[args.indexOf("-o") + 1], fakeServer, { mode: 0o755 });
  if (phase === "build" && process.env.FIXTURE_PHASE === "server") {
    buildServer();
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

  let descendant;
  if (phase === "build" || process.env.FIXTURE_STOP !== "wait") {
    descendant = spawn(process.execPath, [fixture, "--fixture", "descendant"], {
      stdio: ["ignore", "inherit", "inherit", "ipc"],
    });
    await once(descendant, "message");
  }

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
    descendant: descendant?.pid,
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

function killIfPresent(pid) {
  try {
    process.kill(pid, "SIGKILL");
  } catch (error) {
    if (error.code !== "ESRCH") throw error;
  }
}

function startLauncher(t, phase, stop) {
  const dir = mkdtempSync(join(tmpdir(), "sandrun-api-forced-stop-"));
  const buildTmp = join(dir, "builds");
  const exitFile = join(dir, "exit");
  mkdirSync(buildTmp);
  writeFileSync(
    join(dir, "go"),
    '#!/bin/sh\nexec "$FIXTURE_NODE" "$FIXTURE_SCRIPT" --fixture build "$@"\n',
    { mode: 0o755 },
  );
  const task = spawn(process.execPath, [launcher], {
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
    env: {
      ...process.env,
      DATABASE_URL: "postgres://launcher-test",
      PATH: dir + delimiter + process.env.PATH,
      TMPDIR: buildTmp,
      FIXTURE_SCRIPT: fixture,
      FIXTURE_NODE: process.execPath,
      FIXTURE_PHASE: phase,
      FIXTURE_STOP: stop,
      FIXTURE_EXIT: exitFile,
    },
  });
  const run = { task, buildTmp, exitFile, records: [], stderr: "", exit: null };
  const targets = new Set([-task.pid]);
  t.after(() => {
    for (const target of targets) killIfPresent(target);
    rmSync(dir, { recursive: true, force: true });
  });
  createInterface({ input: task.stdout }).on("line", (line) => {
    const record = JSON.parse(line);
    run.records.push(record);
    if (record.event === "ready") {
      const group = Number(ps("pgid", record.pid));
      if (group > 1) targets.add(-group);
      targets.add(record.pid);
      if (record.descendant) targets.add(record.descendant);
    }
  });
  task.stderr.setEncoding("utf8").on("data", (chunk) => (run.stderr += chunk));
  // "close" can wait for pipes that a surviving phase inherited.
  run.exited = once(task, "exit").then(([code, signal]) => {
    run.exit = { code, signal };
    return run.exit;
  });
  run.closed = once(task, "close");
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

async function assertPortsReleased(ports) {
  for (const port of ports) {
    const probe = createServer().listen(port, "127.0.0.1");
    await once(probe, "listening");
    await new Promise((resolve) => probe.close(resolve));
  }
}

if (process.argv[2] === "--fixture") {
  await runFixture(process.argv[3], process.argv.slice(4));
} else {
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

        process.kill(-run.task.pid, "SIGKILL");
        assert.deepEqual(await run.exited, { code: null, signal: "SIGKILL" });
        await waitFor(
          run,
          () => !isRunning(active.pid) && !isRunning(active.descendant),
          "the phase and its descendant to stop",
          stop === "exit" ? 4_000 : 15_000,
        );
        await run.closed;

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
        if (phase === "server") {
          assert.equal(active.ports.length, 2);
          await assertPortsReleased(active.ports);
        }
      },
    );
  }

  for (const [phase, signal, code] of [
    ["server", "SIGINT", 130],
    ["server", "SIGTERM", 143],
    ["server", "SIGQUIT", 131],
  ]) {
    test(
      `${signal} reaches the ${phase} once and the launcher waits (status ${code})`,
      options,
      async (t) => {
        const run = startLauncher(t, phase, "wait");
        const active = await ready(run);

        process.kill(
          signal === "SIGTERM" ? run.task.pid : -run.task.pid,
          signal,
        );
        await waitFor(
          run,
          () => observedSignals(run).length,
          "the forwarded signal",
        );
        assert.equal(run.exit, null, "the launcher exited before its phase");
        assert.ok(isRunning(active.pid));
        writeFileSync(run.exitFile, "");

        assert.deepEqual(await run.exited, { code, signal: null });
        await run.closed;
        assert.deepEqual(observedSignals(run), [signal]);
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
