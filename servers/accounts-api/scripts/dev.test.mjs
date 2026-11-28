// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
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

// freePort picks ports in [PortRangeStart, PortRangeEnd). The range is below
// the ephemeral port ranges of Linux (32768-60999) and macOS (49152-65535).
const PortRangeStart = 20_000;
const PortRangeEnd = 32_768;
const PortAttempts = 100;

// Reserves a free loopback port number. The port is released before returning,
// so a launcher started right after can bind it. The server binds the port only
// after the launcher has built it, which can take seconds. A port number from
// the ephemeral range (`listen(0)`) can go to another process in that time: the
// Go tests that `turbo` runs at the same time in CI open many loopback
// listeners and connections on ephemeral ports. A port below that range goes
// only to a process that asks for that port number.
async function freePort() {
  for (let attempt = 0; attempt < PortAttempts; attempt++) {
    const port =
      PortRangeStart +
      Math.floor(Math.random() * (PortRangeEnd - PortRangeStart));
    const probe = createServer();
    const bound = await new Promise((resolve) => {
      probe.once("error", () => resolve(false));
      probe.listen(port, "127.0.0.1", () => resolve(true));
    });
    if (bound) {
      await new Promise((resolve) => probe.close(resolve));
      return port;
    }
  }
  throw new Error(`no free loopback port after ${PortAttempts} attempts`);
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
  // Keep the fixture independent of the host's telemetry configuration.
  for (const name of Object.keys(childEnv)) {
    if (name.startsWith("OTEL_")) delete childEnv[name];
  }
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
// drain that ends after DrainMs with server_shutdown_completed in timed mode.
// Hold mode keeps the drain open until the second SIGINT. In either mode, a
// second SIGINT ends the drain without a completion event.
const DrainMs = 1500;
const countingServer = `#!${process.execPath}
const mode = process.env.FAKE_MODE;
let sigints = 0;
process.on("SIGINT", () => {
  sigints += 1;
  console.log(JSON.stringify({ event: "child_sigint", n: sigints }));
  if (sigints > 1) process.exit(2);
  if (mode === "hold") return;
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

function serverExitError(stdout) {
  const record = stdout
    .split("\n")
    .filter((line) => line.trim())
    .map((line) => JSON.parse(line))
    .find((record) => record.msg === "server_exited");
  assert.ok(record, `missing server_exited record in stdout: ${stdout}`);
  return record.error;
}

const childSigintCounts = (output) =>
  output
    .split("\n")
    // The fragment after the last newline can still be incomplete.
    .slice(0, -1)
    .filter((line) => line.includes('"event":"child_sigint"'))
    .map((line) => JSON.parse(line).n);

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
  "the build and server receive only explicit OpenTelemetry settings",
  { skip, timeout: Timeout },
  async (t) => {
    const inherited = {
      OTEL_EXPORTER_OTLP_ENDPOINT: "http://127.0.0.1:1",
      OTEL_EXPORTER_OTLP_METRICS_ENDPOINT: "http://127.0.0.1:1/v1/metrics",
      OTEL_EXPORTER_OTLP_LOGS_ENDPOINT: "http://127.0.0.1:1/v1/logs",
      OTEL_EXPORTER_OTLP_HEADERS: "test=generic",
      OTEL_EXPORTER_OTLP_METRICS_HEADERS: "test=metrics",
      OTEL_EXPORTER_OTLP_LOGS_HEADERS: "test=logs",
      OTEL_RESOURCE_ATTRIBUTES: "invalid_fixture_attribute",
      OTEL_METRIC_EXPORT_INTERVAL: "100",
      OTEL_SDK_DISABLED: "true",
    };
    const names = Object.keys(inherited);
    for (const [name, value] of Object.entries(inherited)) {
      const previous = process.env[name];
      t.after(() => {
        if (previous === undefined) delete process.env[name];
        else process.env[name] = previous;
      });
      process.env[name] = value;
    }

    const probe = `#!${process.execPath}
const names = ${JSON.stringify(names)};
const present = Object.fromEntries(names.map((name) => [name, Object.hasOwn(process.env, name)]));
console.log(JSON.stringify({ present, metricsDisabled: process.env.METRICS_ADDR === "" }));
`;
    const pathPrefix = fakeGo(
      t,
      [
        'while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done',
        `cat > "$out" <<'PROGRAM'\n${probe}PROGRAM`,
        'chmod 755 "$out"',
        '"$out"',
      ].join("\n"),
    );
    const endpoint = "OTEL_EXPORTER_OTLP_ENDPOINT";
    for (const env of [
      {},
      {
        [endpoint]: "http://127.0.0.1:2",
        OTEL_EXPORTER_OTLP_LOGS_HEADERS: undefined,
      },
    ]) {
      const run = startLauncher(t, { port: await freePort(), env, pathPrefix });
      assert.equal((await run.exited).code, 0, run.stderr);
      const expected = {
        present: Object.fromEntries(
          names.map((name) => [name, env[name] !== undefined]),
        ),
        metricsDisabled: true,
      };
      // The fake build runs the probe once and the launcher runs it again.
      const records = run.stdout
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line));
      assert.deepEqual(records, [expected, expected]);
    }

    for (const [name, value] of Object.entries(inherited)) {
      assert.ok(process.env[name] === value, `parent ${name} changed`);
    }
  },
);

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
      env: { FAKE_MODE: "timed" },
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
      env: { FAKE_MODE: "hold" },
      pathPrefix: fakeGoBuildingCountingServer(t),
    });
    await waitReady(run);

    process.kill(-run.child.pid, "SIGINT");
    await waitFor(
      run,
      () => {
        assert.equal(
          run.child.exitCode ?? run.child.signalCode,
          null,
          "launcher exited before the second SIGINT",
        );
        return childSigintCounts(run.stdout).includes(1);
      },
      "the first child_sigint record",
    );
    process.kill(-run.child.pid, "SIGINT");
    const { code } = await run.exited;

    assert.equal(code, StatusSigint, run.stderr);
    assert.deepEqual(childSigintCounts(run.stdout), [1, 2], run.stdout);
    assert.doesNotMatch(run.stdout, /"server_shutdown_completed"/);
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
    const started = JSON.parse(
      run.stdout.split("\n").find((line) => line.includes('"server_started"')),
    );
    assert.equal(started.otlp_metrics_export, false);
    assert.equal(started.otlp_logs_export, false);
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
  "an explicitly set malformed OpenTelemetry resource fails startup",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    const run = startLauncher(t, {
      port,
      env: { OTEL_RESOURCE_ATTRIBUTES: "invalid_fixture_attribute" },
    });
    assert.equal((await run.exited).code, 1, run.stderr);
    assert.match(serverExitError(run.stdout), /missing value/);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
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
    assert.match(
      serverExitError(run.stdout),
      new RegExp(
        `^listen tcp 127\\.0\\.0\\.1:${port}: .*address already in use$`,
      ),
    );
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
    assert.equal(
      serverExitError(run.stdout),
      'LOG_LEVEL must be one of debug, info, warn, error; got "bogus"',
    );
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

// The extensionless build output can run under either Node.js package type.
function executionMarkerProgram(marker) {
  return `#!${process.execPath}
import("node:fs").then(({ writeFileSync }) => {
  writeFileSync(${JSON.stringify(marker)}, "");
});
`;
}

for (const type of ["module", "commonjs"]) {
  test(
    `the fake server writes its execution marker under ${type} scope`,
    { skip, timeout: Timeout },
    (t) => {
      const dir = mkdtempSync(join(tmpdir(), "accounts-api-execution-"));
      t.after(() => rmSync(dir, { recursive: true, force: true }));
      const executable = join(dir, "accounts-api");
      const marker = join(dir, "executed");
      writeFileSync(join(dir, "package.json"), JSON.stringify({ type }));
      writeFileSync(executable, executionMarkerProgram(marker), {
        mode: 0o755,
      });

      execFileSync(executable);
      assert.ok(existsSync(marker), "the fake server did not write its marker");
    },
  );
}

// A fake `go` for the tests that signal the launcher during the build. It
// installs its signal trap before it publishes the ready marker, so a test that
// waits for the marker never signals a build that cannot handle the signal yet.
// On the signal it completes a build and exits 0: it writes a runnable program
// to the -o path, then records "build-ok" as its last step. The program records
// "executed" when it starts. The markers live in a test-owned directory outside
// the launcher's TMPDIR (which the launcher's own cleanup assertion inspects).
// The launcher reports the signal status whether or not the build succeeded and
// whether or not the built program ran, so the tests assert both markers:
// without them a failed build and a launcher that runs the program after the
// signal would pass every other assertion.
function signalDuringBuildFixture(t, signal) {
  const markerDir = mkdtempSync(join(tmpdir(), "accounts-api-marker-"));
  t.after(() => rmSync(markerDir, { recursive: true, force: true }));
  const markers = {
    ready: join(markerDir, "trap-ready"),
    buildOk: join(markerDir, "build-ok"),
    executed: join(markerDir, "executed"),
  };
  const program = executionMarkerProgram(markers.executed);
  const script = [
    'while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done',
    "sleep 60 &",
    "pid=$!",
    "finish() {",
    "  kill $pid",
    `  cat > "$out" <<'PROGRAM'\n${program}PROGRAM`,
    '  chmod 755 "$out" && touch "$BUILD_OK"',
    "  exit 0",
    "}",
    `trap finish ${signal}`,
    'touch "$READY"',
    "wait $pid",
  ].join("\n");
  return {
    markers,
    env: { READY: markers.ready, BUILD_OK: markers.buildOk },
    pathPrefix: fakeGo(t, script),
  };
}

function assertBuiltAndNeverRan(markers) {
  assert.equal(
    existsSync(markers.buildOk),
    true,
    "the signal handler did not complete a successful build",
  );
  assert.equal(
    existsSync(markers.executed),
    false,
    "the built program ran after the signal",
  );
}

test(
  "SIGTERM during the build never starts the server",
  { skip, timeout: Timeout * 2 },
  async (t) => {
    const port = await freePort();
    // The fake go waits, then on SIGTERM completes a build and exits 0. A
    // launcher that ignored the termination would run the built program next.
    const { markers, env, pathPrefix } = signalDuringBuildFixture(t, "TERM");
    const run = startLauncher(t, { port, env, pathPrefix });
    await waitFor(run, () => existsSync(markers.ready), "the build trap");

    process.kill(run.child.pid, "SIGTERM");
    const { code } = await run.exited;

    assert.equal(code, StatusSigterm, run.stderr);
    assertBuiltAndNeverRan(markers);
    assert.doesNotMatch(run.stdout, /"server_started"/);
    await assertNoListener(port);
    assert.deepEqual(readdirSync(run.tmp), [], "build output was not removed");
  },
);

for (const [signal, status] of [
  ["INT", StatusSigint],
  ["QUIT", StatusSigquit],
]) {
  test(
    `a process-group SIG${signal} during the build never starts the server`,
    { skip, timeout: Timeout * 2 },
    async (t) => {
      const port = await freePort();
      const { markers, env, pathPrefix } = signalDuringBuildFixture(t, signal);
      const run = startLauncher(t, { port, env, pathPrefix });
      await waitFor(run, () => existsSync(markers.ready), "the build trap");

      process.kill(-run.child.pid, `SIG${signal}`);
      const { code } = await run.exited;

      assert.equal(code, status, run.stderr);
      assert.equal(countEvents(run.stderr, "accounts_api_dev_signal"), 1);
      assertBuiltAndNeverRan(markers);
      assert.doesNotMatch(run.stdout, /"server_started"/);
      await assertNoListener(port);
      assert.deepEqual(
        readdirSync(run.tmp),
        [],
        "build output was not removed",
      );
    },
  );
}

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

// A fake `go` whose build leaves a background `sleep` in the build's process
// group, like the signal fixtures do, but never cleans it up itself: only the
// test teardown can end it. It publishes "<shell pid> <sleep pid>" in a file of
// a test-owned directory (written to a temporary name first and renamed, so a
// reader never sees a partial line). The sleep inherits the launcher's output
// pipes, so the launcher's `close` event cannot settle while it runs; its length
// is far above every test timeout, so a teardown that misses it fails the test
// instead of waiting it out. With `ignoreTerm` the shell and the sleep ignore
// SIGTERM (an ignored disposition survives exec), so the supervisor's SIGTERM
// after the launcher died does not end them and only a group SIGKILL does.
function backgroundDescendantFixture(t, { ignoreTerm = false } = {}) {
  const markerDir = mkdtempSync(join(tmpdir(), "accounts-api-marker-"));
  const pidsFile = join(markerDir, "pids");
  const script = [
    ...(ignoreTerm ? ["trap '' TERM"] : []),
    "sleep 300 &",
    'echo "$$ $!" > "$PIDS_FILE.tmp" && mv "$PIDS_FILE.tmp" "$PIDS_FILE"',
    "wait",
  ].join("\n");
  const fixture = {
    pids: [],
    env: { PIDS_FILE: pidsFile },
    pathPrefix: fakeGo(t, script),
    // Resolves once the fake build has published its pids and records them.
    async waitForPids(run) {
      await waitFor(run, () => existsSync(pidsFile), "the build descendant");
      fixture.pids = readFileSync(pidsFile, "utf8")
        .trim()
        .split(" ")
        .map(Number);
      return fixture.pids;
    },
  };
  // Independent of the launcher cleanup under test: a failed assertion must not
  // leak the sleep, so this ends the recorded processes and the build's group
  // on its own. Its own failure to find them (already gone) is not an error.
  t.after(() => {
    const groups = fixture.pids.map((pid) => {
      try {
        return Number(
          execFileSync("ps", ["-o", "pgid=", "-p", String(pid)], {
            encoding: "utf8",
          }).trim(),
        );
      } catch {
        return 0;
      }
    });
    for (const target of [
      ...groups.filter(Boolean).map((g) => -g),
      ...fixture.pids,
    ]) {
      try {
        process.kill(target, "SIGKILL");
      } catch {
        // Already gone.
      }
    }
    rmSync(markerDir, { recursive: true, force: true });
  });
  return fixture;
}

// Resolves once the launcher's `close` event fired, which needs every output
// pipe closed. A leftover descendant that holds a pipe keeps it pending.
async function waitClosed(run) {
  let closed = false;
  run.exited.then(() => (closed = true));
  await waitFor(run, () => closed, "the launcher's output pipes to close");
}

test(
  "the test cleanup kills a background descendant of the build group",
  { skip, timeout: Timeout },
  async (t) => {
    const port = await freePort();
    const fixture = backgroundDescendantFixture(t);
    const run = startLauncher(t, {
      port,
      env: fixture.env,
      pathPrefix: fixture.pathPrefix,
    });
    const pids = await fixture.waitForPids(run);
    assert.ok(pids.every(isRunning), "the fake build did not start");

    // No signal reaches the fake build's trap or the launcher beforehand: the
    // shell and the sleep end only through the cleanup's group kill.
    stopLauncher(run);

    await waitClosed(run);
    await waitGone(run, pids, "the build and its descendant to be killed");
    assert.equal(existsSync(run.tmp), false, "the test TMPDIR was not removed");
    await assertNoListener(port);
  },
);

test(
  "the test cleanup kills a recorded build group after the launcher died",
  { skip, timeout: Timeout },
  async (t) => {
    const port = await freePort();
    // The shell and the sleep ignore the supervisor's SIGTERM, so after the
    // launcher died they live until the supervisor's SIGKILL, five seconds later.
    const fixture = backgroundDescendantFixture(t, { ignoreTerm: true });
    const run = startLauncher(t, {
      port,
      env: fixture.env,
      pathPrefix: fixture.pathPrefix,
    });
    const pids = await fixture.waitForPids(run);
    recordChildren(run);
    assert.ok(run.childPids.size > 0, "no child of the launcher was recorded");

    // The launcher's own group holds the launcher only, so the build group, with
    // the descendant that holds the output pipes, survives its death.
    process.kill(-run.child.pid, "SIGKILL");
    assert.ok(pids.every(isRunning), "the build group ended with the launcher");

    // The launcher is gone, so the cleanup finds no children of it and has only
    // the recorded pids to work with.
    stopLauncher(run);

    await waitClosed(run);
    await waitGone(run, pids, "the build and its descendant to be killed");
    assert.equal(existsSync(run.tmp), false, "the test TMPDIR was not removed");
    await assertNoListener(port);
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
