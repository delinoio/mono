// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFile, execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
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
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import test from "node:test";
import { resolveChildEnv } from "./dev.mjs";

// These tests start the real launcher without a database and with a fake
// `go` whose build output prints the environment it received.
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const Timeout = 120_000;
// Allow the POSIX supervisor's five-second grace period plus time for stdio.
const CleanupTimeout = 10_000;
// The tests that put a fake `go` first on PATH run a POSIX shell script, so
// Windows is skipped like the accounts-api launcher tests.
const skip = process.platform === "win32";
const ProductionOrigin = "https://sandrun.delino.io";
const DefaultCorsOrigins = [
  ProductionOrigin,
  "http://127.0.0.1:13400",
  "https://sandrun.delino.localhost",
  "http://sandrun.delino.localhost",
].join(",");

const DefaultSettings = {
  ALLOW_DIRECT_CLIENT_ADDRESS: "true",
  CORS_ORIGINS: DefaultCorsOrigins,
  METRICS_ADDR: "127.0.0.1:19400",
};

for (const [label, spelling] of [
  ["uppercase", (name) => name],
  ["lowercase", (name) => name.toLowerCase()],
  ["mixed case", (name) => name[0] + name.slice(1).toLowerCase()],
]) {
  test(`Windows preserves explicit ${label} settings`, () => {
    const env = Object.freeze({
      [spelling("ALLOW_DIRECT_CLIENT_ADDRESS")]: " false ",
      [spelling("CORS_ORIGINS")]: " https://a.example ",
      [spelling("METRICS_ADDR")]: " 127.0.0.1:9999 ",
      Unrelated: "keep",
    });

    assert.deepEqual(resolveChildEnv(env, "win32"), env);
  });

  test(`Windows applies defaults to blank ${label} settings`, () => {
    const blank = {};
    const expected = {};
    for (const [name, value] of Object.entries(DefaultSettings)) {
      blank[spelling(name)] = " \t\n\v\f\r";
      expected[spelling(name)] = value;
    }

    assert.deepEqual(resolveChildEnv(blank, "win32"), expected);
  });
}

test("Windows applies defaults to absent settings", () => {
  assert.deepEqual(resolveChildEnv({}, "win32"), DefaultSettings);
});

test("Windows keeps one explicit value per setting in spawn sort order", () => {
  assert.deepEqual(
    resolveChildEnv(
      {
        allow_direct_client_address: "true",
        ALLOW_DIRECT_CLIENT_ADDRESS: "false",
        Cors_Origins: "https://b.example",
        CORS_ORIGINS: "",
        cors_origins: "https://a.example",
        METRICS_ADDR: "\t",
        metrics_addr: "\u00a0",
      },
      "win32",
    ),
    {
      ALLOW_DIRECT_CLIENT_ADDRESS: "false",
      Cors_Origins: "https://b.example",
      metrics_addr: "\u00a0",
    },
  );
});

test("POSIX keeps differently cased settings distinct", () => {
  const env = {
    allow_direct_client_address: "false",
    Cors_Origins: "https://a.example",
    metrics_addr: "127.0.0.1:9999",
  };

  assert.deepEqual(resolveChildEnv(env, "linux"), {
    ...env,
    ...DefaultSettings,
  });
});

function start(t, env, buildTmp) {
  const child = spawn(process.execPath, [launcher], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
    shell: false,
  });
  let output = "";
  child.stdout.on("data", (d) => (output += d));
  child.stderr.on("data", (d) => (output += d));
  const closed = once(child, "close").then(([code, signal]) => ({
    code,
    signal,
  }));
  t.after(async () => {
    let timer;
    try {
      if (child.pid && child.exitCode === null && child.signalCode === null) {
        // Killing the launcher closes the supervisor's private channel and
        // makes it stop the entire phase group on POSIX.
        child.kill("SIGKILL");
      }
      const deadline = new Promise((_, reject) => {
        timer = setTimeout(() => {
          const error = new Error(
            `launcher output did not close within ${CleanupTimeout}ms\n${output}`,
          );
          // Node reports the test timeout in preference to a teardown error.
          t.diagnostic(error.message);
          reject(error);
        }, CleanupTimeout);
      });
      await Promise.race([closed, deadline]);
    } finally {
      clearTimeout(timer);
      // Release our ends even if an inherited pipe missed the cleanup deadline.
      child.stdout.destroy();
      child.stderr.destroy();
      child.unref();
      if (buildTmp) rmSync(buildTmp, { recursive: true, force: true });
    }
  });
  return { child, closed, output: () => output };
}

test("skips without DATABASE_URL", { timeout: Timeout }, async (t) => {
  const env = { ...process.env };
  delete env.DATABASE_URL;
  const run = start(t, env);
  const { code } = await run.closed;
  assert.equal(code, 0);
  assert.match(run.output(), /sandrun_api_dev_skipped/);
});

function readPids(file) {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .trim()
    .split(/\s+/)
    .map(Number)
    .filter((pid) => Number.isInteger(pid) && pid > 1);
}

function runningPids(pids) {
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
        // An orphaned zombie has exited and holds no output pipe.
        return state && !state.startsWith("Z") ? [Number(pid)] : [];
      });
  } catch (error) {
    if (error.status !== 1) throw error;
    return [];
  }
}

const TimeoutProbe = "stalled launcher fixture";
const ProbeTimeout = 3_000;
const probeDir = process.env.SANDRUN_LAUNCHER_TIMEOUT_PROBE;
if (probeDir) {
  test(TimeoutProbe, { skip, timeout: ProbeTimeout }, async (t) => {
    const buildTmp = join(probeDir, "builds");
    const run = start(t, { ...process.env, TMPDIR: buildTmp }, buildTmp);
    writeFileSync(join(probeDir, "launcher-pid"), String(run.child.pid));
    await run.closed;
  });
} else {
  test(
    "a fixture timeout stops the launcher and clears its build output",
    { skip, timeout: Timeout },
    async (t) => {
      const dir = mkdtempSync(join(tmpdir(), "sandrun-api-timeout-probe-"));
      const buildTmp = join(dir, "builds");
      mkdirSync(buildTmp);
      const fixturePids = join(dir, "fixture-pids");
      const getPids = () => [
        ...readPids(join(dir, "launcher-pid")),
        ...readPids(fixturePids),
      ];
      const goDir = fakeGo(
        t,
        [
          // The build and its descendant hold inherited output through SIGTERM.
          "trap '' TERM",
          "sleep 90 &",
          'printf "%s %s %s\\n" "$PPID" "$$" "$!" > "$FIXTURE_PIDS.tmp"',
          'mv "$FIXTURE_PIDS.tmp" "$FIXTURE_PIDS"',
          "wait",
        ].join("\n"),
      );
      const env = { ...process.env };
      // Run an independent TAP reporter instead of sending events to our runner.
      delete env.NODE_TEST_CONTEXT;
      try {
        // The subprocess must report an actual node:test timeout. The independent
        // deadline and PID cleanup also bound the regression if the fixture leaks.
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
              env: {
                ...env,
                DATABASE_URL: "postgres://launcher-test",
                PATH: `${goDir}${delimiter}${process.env.PATH}`,
                SANDRUN_LAUNCHER_TIMEOUT_PROBE: dir,
                FIXTURE_PIDS: fixturePids,
              },
              timeout: ProbeTimeout + CleanupTimeout + 10_000,
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
        assert.equal(
          existsSync(buildTmp),
          false,
          "fixture build directory survived",
        );
      } finally {
        const pids = getPids();
        for (const pid of pids.length ? runningPids(pids) : []) {
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

function fakeGo(t, script) {
  const dir = mkdtempSync(join(tmpdir(), "sandrun-api-fake-go-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "go");
  writeFileSync(file, `#!/bin/sh\n${script}\n`);
  chmodSync(file, 0o755);
  return dir;
}

// A fake `go` whose "build" writes a program that prints the variables it
// received and exits.
function fakeGoBuildingEnvPrinter(t) {
  // A shebang cannot preserve spaces in the Node executable path.
  const program = `#!/bin/sh
exec "$FIXTURE_NODE" - <<'JS'
console.log(JSON.stringify({ event: "env", database: process.env.DATABASE_URL, cors: process.env.CORS_ORIGINS, direct: process.env.ALLOW_DIRECT_CLIENT_ADDRESS, metrics: process.env.METRICS_ADDR }));
JS
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

// Runs the launcher with a test-owned TMPDIR, so a build directory is visible.
async function runLauncher(t, { env, goDir }) {
  const tmp = mkdtempSync(join(tmpdir(), "sandrun-api-dev-test-"));
  const childEnv = {
    ...process.env,
    PATH: `${goDir}${delimiter}${process.env.PATH}`,
    TMPDIR: tmp,
    FIXTURE_NODE: process.execPath,
  };
  delete childEnv.DATABASE_URL;
  delete childEnv.CORS_ORIGINS;
  delete childEnv.ALLOW_DIRECT_CLIENT_ADDRESS;
  delete childEnv.METRICS_ADDR;
  Object.assign(childEnv, env);
  const run = start(t, childEnv, tmp);
  const { code } = await run.closed;
  return { code, output: run.output(), tmp };
}

const envRecord = (output) =>
  JSON.parse(output.split("\n").find((line) => line.includes('"event":"env"')));

for (const [label, value] of [
  ["empty", ""],
  ["ASCII white space", " \t\n\v\f\r"],
]) {
  test(
    `DATABASE_URL that is ${label} skips and starts no build`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output, tmp } = await runLauncher(t, {
        env: { DATABASE_URL: value },
        // A build would fail with this status; the launcher must not run it.
        goDir: fakeGo(t, "exit 3"),
      });

      assert.equal(code, 0, output);
      assert.match(output, /sandrun_api_dev_skipped/);
      assert.deepEqual(readdirSync(tmp), [], "a build directory was created");
    },
  );

  test(
    `CORS_ORIGINS, ALLOW_DIRECT_CLIENT_ADDRESS and METRICS_ADDR that are ${label} use the defaults`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: {
          DATABASE_URL: "postgres://launcher-test",
          CORS_ORIGINS: value,
          ALLOW_DIRECT_CLIENT_ADDRESS: value,
          METRICS_ADDR: value,
        },
        goDir: fakeGoBuildingEnvPrinter(t),
      });

      assert.equal(code, 0, output);
      assert.deepEqual(envRecord(output), {
        event: "env",
        database: "postgres://launcher-test",
        cors: DefaultCorsOrigins,
        direct: "true",
        metrics: "127.0.0.1:19400",
      });
    },
  );
}

for (const [label, value] of [
  ["U+00A0", "\u00a0"],
  ["U+3000", "\u3000"],
]) {
  test(
    `DATABASE_URL that is only ${label} reaches the child unchanged`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: { DATABASE_URL: value },
        goDir: fakeGoBuildingEnvPrinter(t),
      });

      assert.equal(code, 0, output);
      assert.doesNotMatch(output, /sandrun_api_dev_skipped/);
      assert.equal(envRecord(output).database, value);
    },
  );

  test(
    `CORS_ORIGINS, ALLOW_DIRECT_CLIENT_ADDRESS and METRICS_ADDR that are only ${label} reach the child unchanged`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: {
          DATABASE_URL: "postgres://launcher-test",
          CORS_ORIGINS: value,
          ALLOW_DIRECT_CLIENT_ADDRESS: value,
          METRICS_ADDR: value,
        },
        goDir: fakeGoBuildingEnvPrinter(t),
      });

      assert.equal(code, 0, output);
      assert.deepEqual(envRecord(output), {
        event: "env",
        database: "postgres://launcher-test",
        cors: value,
        direct: value,
        metrics: value,
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
      goDir: fakeGoBuildingEnvPrinter(t),
    });

    assert.equal(code, 0, output);
    assert.deepEqual(envRecord(output), {
      event: "env",
      database: "postgres://launcher-test",
      cors: DefaultCorsOrigins,
      direct: "true",
      metrics: "127.0.0.1:19400",
    });
  },
);

test(
  "explicit values pass through unchanged",
  { skip, timeout: Timeout },
  async (t) => {
    const { code, output } = await runLauncher(t, {
      env: {
        DATABASE_URL: " postgres://launcher-test ",
        CORS_ORIGINS: " https://a.example ",
        ALLOW_DIRECT_CLIENT_ADDRESS: " false ",
        METRICS_ADDR: " 127.0.0.1:9999 ",
      },
      goDir: fakeGoBuildingEnvPrinter(t),
    });

    assert.equal(code, 0, output);
    assert.deepEqual(envRecord(output), {
      event: "env",
      database: " postgres://launcher-test ",
      cors: " https://a.example ",
      direct: " false ",
      metrics: " 127.0.0.1:9999 ",
    });
  },
);
