// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFile, execFileSync, spawn } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import test from "node:test";

// These tests start the real launcher without a database. The test that runs
// the real server through the launcher needs Postgres and owns a generated
// database, so it is a Go test:
// TestLauncherForwardsSIGTERMToServerOnGeneratedDatabase in
// ../cmd/ownsand-api/launcher_signal_test.go.
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const Timeout = 120_000;
// Allow the POSIX supervisor's five-second grace period plus time for stdio.
const TeardownTimeout = 10_000;
// The tests that put a fake `go` first on PATH run a POSIX shell script, so
// Windows is skipped like the accounts-api launcher tests.
const skip = process.platform === "win32";
const ProductionOrigin = "https://ownsand.delino.io";
const DefaultCorsOrigins = [
  ProductionOrigin,
  "http://127.0.0.1:13300",
  "https://ownsand.delino.localhost",
  "http://ownsand.delino.localhost",
].join(",");

function start(t, env, scratch) {
  const child = spawn(process.execPath, [launcher], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
    shell: false,
  });
  let output = "";
  child.stdout.on("data", (d) => (output += d));
  child.stderr.on("data", (d) => (output += d));
  child.on("error", (error) => (output += `${error}\n`));
  const closed = new Promise((resolve) => {
    child.once("close", (code, signal) => resolve({ code, signal }));
  });
  t.after(async () => {
    let timer;
    try {
      if (child.pid && child.exitCode === null && child.signalCode === null) {
        // The launcher holds the supervisor's lifetime channel. Killing the
        // launcher makes the POSIX supervisor stop the whole phase group.
        child.kill("SIGKILL");
      }
      const deadline = new Promise((_, reject) => {
        timer = setTimeout(() => {
          const error = new Error(
            `launcher output did not close within ${TeardownTimeout}ms\n${output}`,
          );
          // Node reports the test timeout in preference to a teardown error.
          t.diagnostic(error.message);
          reject(error);
        }, TeardownTimeout);
      });
      // The close event waits for both process exit and inherited output pipes.
      await Promise.race([closed, deadline]);
    } finally {
      clearTimeout(timer);
      // A pipe that stays open past the deadline must not keep Node running.
      child.stdout.destroy();
      child.stderr.destroy();
      child.unref();
    }
    // Retain scratch if the deadline expired before the child closed.
    if (scratch) rmSync(scratch, { recursive: true, force: true });
  });
  return { child, closed, output: () => output };
}

test("skips without DATABASE_URL", { timeout: Timeout }, async (t) => {
  const env = { ...process.env };
  delete env.DATABASE_URL;
  const run = start(t, env);
  const { code } = await run.closed;
  assert.equal(code, 0, run.output());
  assert.match(run.output(), /ownsand_api_dev_skipped/);
});

// A fake `go` whose "build" writes a program that prints the variables it
// received and exits.
function envPrinterBuildScript() {
  const program = `#!/bin/sh
exec "$FIXTURE_NODE" - <<'SERVER_JS'
console.log(JSON.stringify({ event: "env", cors: process.env.CORS_ORIGINS, direct: process.env.ALLOW_DIRECT_CLIENT_ADDRESS, database: process.env.DATABASE_URL }));
SERVER_JS
`;
  return [
    'while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done',
    `cat > "$out" <<'PROGRAM'\n${program}PROGRAM`,
    'chmod 755 "$out"',
  ].join("\n");
}

// Runs the launcher with a test-owned TMPDIR, so a build directory is visible.
function startWithFakeGo(t, { env, goScript }) {
  const scratch = mkdtempSync(join(tmpdir(), "ownsand-api-dev-test-"));
  const tmp = join(scratch, "builds");
  const go = join(scratch, "go");
  try {
    mkdirSync(tmp);
    writeFileSync(go, `#!/bin/sh\n${goScript}\n`, { mode: 0o755 });
    const childEnv = {
      ...process.env,
      FIXTURE_NODE: process.execPath,
      PATH: `${scratch}${delimiter}${process.env.PATH}`,
      TMPDIR: tmp,
    };
    delete childEnv.DATABASE_URL;
    delete childEnv.CORS_ORIGINS;
    delete childEnv.ALLOW_DIRECT_CLIENT_ADDRESS;
    Object.assign(childEnv, env);
    return { ...start(t, childEnv, scratch), tmp };
  } catch (error) {
    rmSync(scratch, { recursive: true, force: true });
    throw error;
  }
}

async function runLauncher(t, options) {
  const run = startWithFakeGo(t, options);
  const { code } = await run.closed;
  return { code, output: run.output(), tmp: run.tmp };
}

function readPids(file) {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .trim()
    .split(/\s+/)
    .map(Number)
    .filter((pid) => Number.isInteger(pid) && pid > 1);
}

function runningPids(pids) {
  if (pids.length === 0) return [];
  try {
    const states = execFileSync(
      "ps",
      ["-o", "pid=,stat=", "-p", pids.join(",")],
      { encoding: "utf8" },
    );
    return states
      .trim()
      .split("\n")
      .flatMap((line) => {
        const [pid, state] = line.trim().split(/\s+/);
        // Orphaned zombies have exited and hold no output pipes.
        return state && !state.startsWith("Z") ? [Number(pid)] : [];
      });
  } catch (error) {
    if (error.status !== 1) throw error;
    return [];
  }
}

const TimeoutProbe = "stalled launcher fixture";
const ProbeTimeout = 3_000;
const probeDir = process.env.OWNSAND_LAUNCHER_TIMEOUT_PROBE;

if (probeDir) {
  test(TimeoutProbe, { skip, timeout: ProbeTimeout }, async (t) => {
    const run = startWithFakeGo(t, {
      env: {
        DATABASE_URL: "postgres://launcher-test",
        FIXTURE_PIDS: join(probeDir, "fixture-pids"),
      },
      goScript: [
        // Both processes keep their inherited output pipes open through SIGTERM.
        "trap '' TERM",
        "sleep 90 &",
        'printf "%s %s %s\\n" "$PPID" "$$" "$!" > "$FIXTURE_PIDS.tmp"',
        'mv "$FIXTURE_PIDS.tmp" "$FIXTURE_PIDS"',
        "wait",
      ].join("\n"),
    });
    writeFileSync(join(probeDir, "launcher-pid"), String(run.child.pid));
    await run.closed;
  });
} else {
  test(
    "a fixture timeout stops the launcher and removes its scratch directory",
    { skip, timeout: Timeout },
    async () => {
      const dir = mkdtempSync(join(tmpdir(), "ownsand-api-timeout-probe-"));
      const getPids = () => [
        ...readPids(join(dir, "launcher-pid")),
        ...readPids(join(dir, "fixture-pids")),
      ];
      const env = {
        ...process.env,
        OWNSAND_LAUNCHER_TIMEOUT_PROBE: dir,
        TMPDIR: dir,
      };
      // The subprocess must report its timeout through its own TAP reporter.
      delete env.NODE_TEST_CONTEXT;
      try {
        await assert.rejects(
          promisify(execFile)(
            process.execPath,
            [
              "--test",
              "--test-isolation=none",
              "--test-reporter=tap",
              `--test-name-pattern=^${TimeoutProbe}$`,
              fileURLToPath(import.meta.url),
            ],
            {
              env,
              timeout: ProbeTimeout + TeardownTimeout + 10_000,
              killSignal: "SIGKILL",
            },
          ),
          (error) => {
            const output = error.stdout + error.stderr;
            assert.equal(error.code, 1, output);
            assert.match(output, /failureType: 'testTimeoutFailure'/);
            assert.doesNotMatch(output, /launcher output did not close/);
            return true;
          },
        );
        const pids = getPids();
        assert.equal(
          new Set(pids).size,
          4,
          "expected launcher, supervisor, build and descendant PIDs",
        );
        const deadline = performance.now() + 1_000;
        let running = runningPids(pids);
        while (running.length && performance.now() < deadline) {
          await delay(25);
          running = runningPids(pids);
        }
        assert.deepEqual(running, [], "fixture processes survived the timeout");
        assert.deepEqual(
          readdirSync(dir).sort(),
          ["fixture-pids", "launcher-pid"],
          "fixture scratch directory survived the timeout",
        );
      } finally {
        // The outer deadline can stop the test subprocess even if the fixture
        // leaks. In that case, kill only the processes the fixture recorded.
        for (const pid of runningPids(getPids())) {
          try {
            process.kill(pid, "SIGKILL");
          } catch (error) {
            if (error.code !== "ESRCH") throw error;
          }
        }
        rmSync(dir, { recursive: true, force: true });
      }
    },
  );
}

const envRecord = (output) =>
  JSON.parse(output.split("\n").find((line) => line.includes('"event":"env"')));

for (const [label, value] of [
  ["empty", ""],
  ["white space", "   "],
]) {
  test(
    `DATABASE_URL that is ${label} skips and starts no build`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output, tmp } = await runLauncher(t, {
        env: { DATABASE_URL: value },
        // A build would fail with this status; the launcher must not run it.
        goScript: "exit 3",
      });

      assert.equal(code, 0, output);
      assert.match(output, /ownsand_api_dev_skipped/);
      assert.deepEqual(readdirSync(tmp), [], "a build directory was created");
    },
  );

  test(
    `CORS_ORIGINS and ALLOW_DIRECT_CLIENT_ADDRESS that are ${label} use the defaults`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: {
          DATABASE_URL: "postgres://launcher-test",
          CORS_ORIGINS: value,
          ALLOW_DIRECT_CLIENT_ADDRESS: value,
        },
        goScript: envPrinterBuildScript(),
      });

      assert.equal(code, 0, output);
      assert.deepEqual(envRecord(output), {
        event: "env",
        cors: DefaultCorsOrigins,
        direct: "true",
        database: "postgres://launcher-test",
      });
    },
  );
}

// The server removes only ASCII white space, so Unicode white space is a value,
// not an unset variable. The launcher must neither skip nor replace it.
for (const [label, value] of [
  ["U+00A0", "\u00a0"],
  ["U+3000", "\u3000"],
]) {
  test(
    `DATABASE_URL that is only ${label} is set and starts the build`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: { DATABASE_URL: value },
        goScript: envPrinterBuildScript(),
      });

      assert.equal(code, 0, output);
      assert.doesNotMatch(output, /ownsand_api_dev_skipped/);
      assert.equal(envRecord(output).database, value);
    },
  );

  test(
    `CORS_ORIGINS and ALLOW_DIRECT_CLIENT_ADDRESS that are only ${label} pass through unchanged`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: {
          DATABASE_URL: "postgres://launcher-test",
          CORS_ORIGINS: value,
          ALLOW_DIRECT_CLIENT_ADDRESS: value,
        },
        goScript: envPrinterBuildScript(),
      });

      assert.equal(code, 0, output);
      assert.deepEqual(envRecord(output), {
        event: "env",
        cors: value,
        direct: value,
        database: "postgres://launcher-test",
      });
    },
  );
}

test(
  "unset development variables use the defaults",
  { skip, timeout: Timeout },
  async (t) => {
    const { code, output } = await runLauncher(t, {
      env: { DATABASE_URL: "postgres://launcher-test" },
      goScript: envPrinterBuildScript(),
    });

    assert.equal(code, 0, output);
    assert.deepEqual(envRecord(output), {
      event: "env",
      cors: DefaultCorsOrigins,
      direct: "true",
      database: "postgres://launcher-test",
    });
  },
);

test(
  "explicit values pass through unchanged",
  { skip, timeout: Timeout },
  async (t) => {
    const { code, output } = await runLauncher(t, {
      env: {
        DATABASE_URL: "postgres://launcher-test",
        CORS_ORIGINS: "https://a.example",
        ALLOW_DIRECT_CLIENT_ADDRESS: "false",
      },
      goScript: envPrinterBuildScript(),
    });

    assert.equal(code, 0, output);
    assert.deepEqual(envRecord(output), {
      event: "env",
      cors: "https://a.example",
      direct: "false",
      database: "postgres://launcher-test",
    });
  },
);
