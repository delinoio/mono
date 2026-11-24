// SPDX-License-Identifier: FSL-1.1-ALv2

// Development launcher for tide-api (`pnpm --filter tide-api dev`).
//
// It builds the server into a temporary directory and runs the binary. Each
// phase runs under a supervisor on POSIX and starts directly on Windows.
// The launcher forwards SIGINT, SIGTERM and SIGQUIT to the active phase,
// waits for it to exit and removes the build output. A signal before the
// server starts stops the launcher without starting the server.
//
// The server needs Postgres and TIDE_SECRET_KEYS. A value that is empty or
// consists only of ASCII white space counts as unset for DATABASE_URL,
// CORS_ORIGINS, ALLOW_DIRECT_CLIENT_ADDRESS and METRICS_ADDR, as it does for
// the server.
// When DATABASE_URL is unset, the launcher prints a `tide_api_dev_skipped`
// record and exits with status 0 instead of failing, so the root `pnpm dev`
// keeps running the other development servers on machines without a
// database. The server listens on its development default
// 127.0.0.1:18400. When METRICS_ADDR is unset, the launcher sets the
// development metrics port 127.0.0.1:19400. When CORS_ORIGINS is unset, it
// allows the console's development origins in addition to the production
// origin: http://127.0.0.1:13400 (the fixed console port) and
// https://tide.delino.localhost and http://tide.delino.localhost (the
// portless URLs that the console will use, like ownsand-console). When
// ALLOW_DIRECT_CLIENT_ADDRESS is unset, it sets it to true: the development
// server is reached directly, so the TCP peer is the client address for rate
// limits. TRUST_PROXY_CLIENT_ADDRESS=true still takes precedence.
//
// Child processes use explicit argument arrays and `shell: false` according
// to the repository environment contract.
import { spawn } from "node:child_process";
import { closeSync, openSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { Socket } from "node:net";
import { constants, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ForwardedSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  Quit: "SIGQUIT",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];
const useSupervisor = process.platform !== "win32";
const SuperviseFlag = "--supervise";
const StopGraceMs = 5000;
const DevelopmentConsoleOrigins = Object.freeze([
  "http://127.0.0.1:13400",
  "https://tide.delino.localhost",
  "http://tide.delino.localhost",
]);
const ProductionConsoleOrigin = "https://tide.delino.io";
const DevelopmentMetricsAddr = "127.0.0.1:19400";

// Match the server's ASCII white space rule when deciding whether a value is unset.
const AsciiWhiteSpaceOnly = /^[\t\n\v\f\r ]*$/;
const isUnset = (value) =>
  value === undefined || AsciiWhiteSpaceOnly.test(value);

const launcherPath = fileURLToPath(import.meta.url);
const packageDir = resolve(dirname(launcherPath), "..");
const binaryName = process.platform === "win32" ? "tide-api.exe" : "tide-api";

let received = null;
let activeChild = null;
function installSignalHandlers() {
  return Object.values(ForwardedSignal).map((signal) => {
    const handler = () => {
      received ??= signal;
      console.error(JSON.stringify({ event: "tide_api_dev_signal", signal }));
      activeChild?.kill(signal);
    };
    process.on(signal, handler);
    return [signal, handler];
  });
}

function logFailure(error) {
  console.error(
    JSON.stringify({ event: "tide_api_dev_failed", error: String(error) }),
  );
}

// As in ownsand-api, the supervisor leads a separate process group on POSIX.
// This lets the launcher forward each terminal signal once. A task-group
// SIGKILL cannot be forwarded, but it closes the private pipe on fd 3. The
// supervisor then asks its whole group to stop, and kills any survivors
// when the phase exits or the grace period expires.
function supervise(command, args) {
  let launcherGone = false;
  let child;
  const stopGroup = (signal) => {
    process.kill(-process.pid, signal);
  };
  for (const signal of Object.values(ForwardedSignal)) {
    process.on(signal, () => {
      // The supervisor receives its own group SIGTERM during a forced stop.
      if (!launcherGone) child.kill(signal);
    });
  }
  const onLauncherGone = () => {
    if (launcherGone) return;
    launcherGone = true;
    console.error(JSON.stringify({ event: "tide_api_dev_launcher_gone" }));
    stopGroup("SIGTERM");
    setTimeout(() => stopGroup("SIGKILL"), StopGraceMs);
  };
  const pipe = new Socket({ fd: 3, readable: true, writable: false });
  pipe.once("end", onLauncherGone);
  pipe.once("error", onLauncherGone);
  pipe.resume();

  // libuv skips ignored descriptors above fd 2. Replace fd 3 explicitly
  // so the build, server and their descendants cannot inherit the pipe.
  const devNull = openSync("/dev/null", "r");
  try {
    child = spawn(command, args, {
      cwd: packageDir,
      stdio: ["inherit", "inherit", "inherit", devNull],
      shell: false,
    });
  } finally {
    closeSync(devNull);
  }
  child.once("error", (error) => {
    logFailure(error);
    process.exit(1);
  });
  child.once("close", (code, signal) => {
    if (launcherGone) stopGroup("SIGKILL");
    process.exit(code ?? (signal ? signalExitCode(signal) : 1));
  });
}

function run(command, args, env) {
  return new Promise((resolvePromise, reject) => {
    const child = useSupervisor
      ? spawn(
          process.execPath,
          [launcherPath, SuperviseFlag, command, ...args],
          {
            cwd: packageDir,
            stdio: ["inherit", "inherit", "inherit", "pipe"],
            shell: false,
            detached: true,
            env,
          },
        )
      : spawn(command, args, {
          cwd: packageDir,
          stdio: "inherit",
          shell: false,
          env,
        });
    // Keep the launcher end open for the phase lifetime. It carries no data.
    const lifetimePipe = child.stdio[3];
    lifetimePipe?.on("error", () => {});
    activeChild = child;
    child.once("error", (error) => {
      activeChild = null;
      reject(error);
    });
    child.once("close", (code, signal) => {
      activeChild = null;
      lifetimePipe?.destroy();
      resolvePromise(code ?? (signal ? signalExitCode(signal) : 1));
    });
  });
}

const stoppedStatus = () => signalExitCode(received);

async function main() {
  if (isUnset(process.env.DATABASE_URL)) {
    console.error(
      JSON.stringify({
        event: "tide_api_dev_skipped",
        reason: "DATABASE_URL is not set",
      }),
    );
    return 0;
  }
  const env = { ...process.env };
  if (isUnset(env.ALLOW_DIRECT_CLIENT_ADDRESS)) {
    env.ALLOW_DIRECT_CLIENT_ADDRESS = "true";
  }
  if (isUnset(env.METRICS_ADDR)) {
    env.METRICS_ADDR = DevelopmentMetricsAddr;
  }
  if (isUnset(env.CORS_ORIGINS)) {
    env.CORS_ORIGINS = [
      ProductionConsoleOrigin,
      ...DevelopmentConsoleOrigins,
    ].join(",");
  }
  const buildDir = await mkdtemp(join(tmpdir(), "tide-api-dev-"));
  try {
    if (received) return stoppedStatus();
    const binary = join(buildDir, binaryName);
    // -buildvcs=false matches the `build` script (VCS discovery can fail in
    // a linked Git worktree).
    const buildStatus = await run(
      "go",
      ["build", "-buildvcs=false", "-o", binary, "./cmd/tide-api"],
      env,
    );
    if (received) return stoppedStatus();
    if (buildStatus !== 0) return buildStatus;
    const serverStatus = await run(binary, [], env);
    return received ? stoppedStatus() : serverStatus;
  } finally {
    await rm(buildDir, { recursive: true, force: true });
  }
}

if (import.meta.main && process.argv[2] === SuperviseFlag) {
  supervise(process.argv[3], process.argv.slice(4));
} else if (import.meta.main) {
  const handlers = installSignalHandlers();
  try {
    process.exitCode = await main();
  } catch (error) {
    logFailure(error);
    process.exitCode = 1;
  } finally {
    for (const [signal, handler] of handlers) process.off(signal, handler);
  }
}
