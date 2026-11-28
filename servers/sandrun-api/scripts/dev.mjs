// SPDX-License-Identifier: FSL-1.1-ALv2

// Development launcher for sandrun-api (`pnpm --filter sandrun-api dev`).
//
// It builds the server into a temporary directory and runs the binary. On
// POSIX, a detached coordinator runs both supervised phases and owns the
// directory. On Windows, the launcher starts both phases directly and owns
// the directory. The launcher waits for shutdown and cleanup before exiting.
// It forwards SIGINT, SIGTERM and SIGQUIT toward the active phase, except for
// SIGINT that the Windows console delivers. A stop before the server starts
// prevents server startup.
//
// The server needs Postgres and SANDRUN_SECRET_KEYS. A value that is empty or
// consists only of ASCII white space counts as unset for DATABASE_URL,
// CORS_ORIGINS, ALLOW_DIRECT_CLIENT_ADDRESS and METRICS_ADDR, as it does for
// the server.
// When DATABASE_URL is unset, the launcher prints a `sandrun_api_dev_skipped`
// record and exits with status 0 instead of failing, so the root `pnpm dev`
// keeps running the other development servers on machines without a
// database. The server listens on its development default
// 127.0.0.1:18400. When METRICS_ADDR is unset, the launcher sets the
// development metrics port 127.0.0.1:19400. When CORS_ORIGINS is unset, it
// allows the console's development origins in addition to the production
// origin: http://127.0.0.1:13400 (the fixed console port) and
// https://sandrun.delino.localhost and http://sandrun.delino.localhost (the
// portless URLs that the console will use, like ownsand-console). When
// ALLOW_DIRECT_CLIENT_ADDRESS is unset, it sets it to true: the development
// server is reached directly, so the TCP peer is the client address for rate
// limits. TRUST_PROXY_CLIENT_ADDRESS=true still takes precedence.
//
// Child processes use explicit argument arrays and `shell: false` according
// to the repository environment contract.
import { spawn } from "node:child_process";
import { once } from "node:events";
import { closeSync, openSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { Socket } from "node:net";
import { constants, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { isWorkspaceCli } from "../../../scripts/ci/workspace-node.mjs";

const ForwardedSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  Quit: "SIGQUIT",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];
const useSupervisor = process.platform !== "win32";
const CoordinateFlag = "--coordinate";
const SuperviseFlag = "--supervise";
const StopGraceMs = 5000;
const Phase = Object.freeze({ Build: "build", Server: "server" });
const BuildMessage = Object.freeze({
  Exited: "exited",
  Finish: "finish",
  ForceStop: "force-stop",
});
const StartPhase = "start-phase";
const DevelopmentConsoleOrigins = Object.freeze([
  "http://127.0.0.1:13400",
  "https://sandrun.delino.localhost",
  "http://sandrun.delino.localhost",
]);
const ProductionConsoleOrigin = "https://sandrun.delino.io";
const DevelopmentMetricsAddr = "127.0.0.1:19400";

// Match the server's ASCII white space rule when deciding whether a value is unset.
const AsciiWhiteSpaceOnly = /^[\t\n\v\f\r ]*$/;
const isUnset = (value) =>
  value === undefined || AsciiWhiteSpaceOnly.test(value);

function applyEnvDefault(env, name, value, platform) {
  // Windows treats case variants as one variable. Use Node's spawn sort order
  // to choose between explicit values, and keep only the chosen spelling.
  const aliases =
    platform === "win32"
      ? Object.keys(env)
          .filter((key) => key.toUpperCase() === name)
          .sort()
      : [];
  const key =
    aliases.find((alias) => !isUnset(env[alias])) ?? aliases[0] ?? name;
  for (const alias of aliases) {
    if (alias !== key) delete env[alias];
  }
  if (isUnset(env[key])) env[key] = value;
}

export function resolveChildEnv(baseEnv, platform = process.platform) {
  const env = { ...baseEnv };
  applyEnvDefault(env, "ALLOW_DIRECT_CLIENT_ADDRESS", "true", platform);
  applyEnvDefault(env, "METRICS_ADDR", DevelopmentMetricsAddr, platform);
  applyEnvDefault(
    env,
    "CORS_ORIGINS",
    [ProductionConsoleOrigin, ...DevelopmentConsoleOrigins].join(","),
    platform,
  );
  return env;
}

const launcherPath = fileURLToPath(import.meta.url);
const packageDir = resolve(dirname(launcherPath), "..");
const binaryName =
  process.platform === "win32" ? "sandrun-api.exe" : "sandrun-api";

let received = null;
let forwardSignal = null;
function requestStop(signal) {
  received ??= signal;
  forwardSignal?.(signal);
}

function installSignalHandlers() {
  return Object.values(ForwardedSignal).map((signal) => {
    const handler = () => {
      console.error(
        JSON.stringify({ event: "sandrun_api_dev_signal", signal }),
      );
      requestStop(signal);
    };
    process.on(signal, handler);
    return [signal, handler];
  });
}

function logFailure(error) {
  console.error(
    JSON.stringify({ event: "sandrun_api_dev_failed", error: String(error) }),
  );
}

// Forced-stop protection (POSIX). A child that runs in its own process group
// is outside the group that a task runner kills, and SIGKILL of the launcher
// cannot be forwarded, so a build or server would survive it and keep its
// port. The coordinator therefore starts each phase through a supervisor: this
// file started again with `--supervise` in a process group of its own, with a
// private channel (file descriptor 3) to the coordinator. The supervisor runs the
// build or server as its child in the same group. The build supervisor reads
// stop requests from the channel and signals its whole group. The server
// supervisor relays OS signals to the server once per delivery and sends
// SIGKILL to the group after the server exits. When the launcher dies, the
// coordinator requests a forced stop through the channel. The supervisor sends
// SIGTERM to its group so a server can drain. A group stop ends with SIGKILL
// when the child exits or the grace period expires. This ends surviving
// descendants. The coordinator keeps its end of the channel. The supervisor
// gives its child /dev/null as descriptor 3 to keep the channel private.
// Windows starts the child directly (`useSupervisor` is false there).
function supervise(phase, command, args) {
  const isBuild = phase === Phase.Build;
  let stoppingGroup = false;
  let serverStopRequested = false;
  let child = null;
  let childStatus = null;
  // The supervisor leads its group (the coordinator starts it detached), so the
  // group id is its own pid and the child and its descendants are members.
  const stopGroup = (signal) => {
    process.kill(-process.pid, signal);
  };
  for (const signal of Object.values(ForwardedSignal)) {
    process.on(signal, () => {
      // A group signal reaches the supervisor too. Only the server supervisor
      // relays OS signals, and only before a group stop.
      if (!isBuild && !stoppingGroup) {
        serverStopRequested = true;
        child?.kill(signal);
      }
    });
  }
  const finish = () => {
    if (stoppingGroup || serverStopRequested) stopGroup("SIGKILL");
    process.exit(childStatus);
  };
  const requestGroupStop = (signal) => {
    if (!stoppingGroup) {
      stoppingGroup = true;
      setTimeout(() => stopGroup("SIGKILL"), StopGraceMs);
    }
    stopGroup(signal);
    if (childStatus !== null) finish();
  };
  const onLauncherGone = () => {
    if (stoppingGroup) return;
    console.error(JSON.stringify({ event: "sandrun_api_dev_launcher_gone" }));
    requestGroupStop("SIGTERM");
  };
  if (isBuild) {
    process.on("disconnect", onLauncherGone);
    process.on("message", (message) => {
      if (message === BuildMessage.Finish) {
        finish();
      } else if (message === BuildMessage.ForceStop) {
        onLauncherGone();
      } else {
        requestGroupStop(message);
      }
    });
  } else {
    const pipe = new Socket({ fd: 3, readable: true, writable: false });
    pipe.once("end", onLauncherGone);
    pipe.once("error", onLauncherGone);
    pipe.resume();
  }

  // Descriptor 3 is the channel here. Spawning with an "ignore" entry would leave
  // it open in the child (libuv skips ignored descriptors above 2), so the
  // child gets /dev/null there instead, which replaces it.
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
    childStatus = code ?? (signal ? signalExitCode(signal) : 1);
    if (isBuild && !stoppingGroup) {
      // Wait for the coordinator to acknowledge the exit. A stop it has already
      // forwarded arrives before that acknowledgment on the same channel.
      process.send(BuildMessage.Exited, (error) => {
        if (error) onLauncherGone();
      });
    } else {
      finish();
    }
  });
}

function run(phase, command, args, env, launcherLifetime) {
  return new Promise((resolvePromise, reject) => {
    // On POSIX the child runs under a supervisor in its own process group (see
    // the forced-stop protection above); the supervisor exits with the child's
    // status, or the shell convention for a fatal signal.
    const superviseBuild = useSupervisor && phase === Phase.Build;
    const child = useSupervisor
      ? spawn(
          process.execPath,
          [launcherPath, SuperviseFlag, phase, command, ...args],
          {
            cwd: packageDir,
            stdio: [
              "inherit",
              "inherit",
              "inherit",
              superviseBuild ? "ipc" : "pipe",
            ],
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
    // The server supervisor uses this pipe only to detect a forced stop.
    const lifetimePipe = child.stdio[3];
    lifetimePipe?.on("error", () => {});
    const forceStop = () => {
      // Keep the build IPC open until the supervisor exits so Node emits close.
      if (superviseBuild) child.send(BuildMessage.ForceStop, () => {});
      else lifetimePipe?.destroy();
    };
    launcherLifetime?.addEventListener("abort", forceStop, { once: true });
    if (superviseBuild) {
      // A signal can arrive as the build supervisor closes. In that case the
      // coordinator has already recorded the signal and will skip the server.
      const send = (message) => child.send(message, () => {});
      child.once("message", (message) => {
        if (message === BuildMessage.Exited) send(BuildMessage.Finish);
      });
      forwardSignal = send;
    } else {
      forwardSignal = (signal) => {
        // The Windows console delivers Ctrl-C to the direct child. Node's
        // kill(SIGINT) would terminate it before its handler could finish.
        if (!useSupervisor && signal === ForwardedSignal.Interrupt) return;
        child.kill(signal);
      };
    }
    child.once("error", (error) => {
      forwardSignal = null;
      reject(error);
    });
    child.once("close", (code, signal) => {
      forwardSignal = null;
      launcherLifetime?.removeEventListener("abort", forceStop);
      lifetimePipe?.destroy();
      resolvePromise(code ?? (signal ? signalExitCode(signal) : 1));
    });
  });
}

const stoppedStatus = () => signalExitCode(received);

async function coordinate() {
  const lifetime = new AbortController();
  let confirmPhase;
  const onLauncherGone = () => {
    lifetime.abort();
    confirmPhase?.(false);
  };
  process.once("disconnect", onLauncherGone);
  process.on("message", (message) => {
    if (message === StartPhase) confirmPhase?.(true);
    else requestStop(message);
  });

  // Require a fresh reply before each phase. The disconnect event may still
  // be pending when the previous phase closes.
  const launcherAllowsPhase = async () => {
    if (!process.connected) return false;
    const { promise, resolve } = Promise.withResolvers();
    confirmPhase = resolve;
    process.send(StartPhase, (error) => {
      if (error) onLauncherGone();
    });
    try {
      return await promise;
    } finally {
      confirmPhase = undefined;
    }
  };

  try {
    return await runPhases(process.env, launcherAllowsPhase, lifetime.signal);
  } finally {
    if (process.connected) process.disconnect();
  }
}

async function runCoordinator(env) {
  const child = spawn(process.execPath, [launcherPath, CoordinateFlag], {
    cwd: packageDir,
    stdio: ["inherit", "inherit", "inherit", "ipc"],
    shell: false,
    detached: true,
    env,
  });
  const send = (message) => child.send(message, () => {});
  // Signal requests and phase replies share one ordered channel.
  forwardSignal = send;
  child.on("message", (message) => {
    if (message === StartPhase) send(StartPhase);
  });
  try {
    const [code, signal] = await once(child, "close");
    return code ?? (signal ? signalExitCode(signal) : 1);
  } finally {
    forwardSignal = null;
  }
}

async function main() {
  if (isUnset(process.env.DATABASE_URL)) {
    console.error(
      JSON.stringify({
        event: "sandrun_api_dev_skipped",
        reason: "DATABASE_URL is not set",
      }),
    );
    return 0;
  }
  const env = resolveChildEnv(process.env);
  return useSupervisor ? runCoordinator(env) : runPhases(env);
}

async function runPhases(env, launcherAllowsPhase, launcherLifetime) {
  const buildDir = await mkdtemp(join(tmpdir(), "sandrun-api-dev-"));
  try {
    if (launcherAllowsPhase && !(await launcherAllowsPhase())) return 1;
    if (received) return stoppedStatus();
    const binary = join(buildDir, binaryName);
    // -buildvcs=false matches the `build` script (VCS discovery can fail in
    // a linked Git worktree).
    const buildStatus = await run(
      Phase.Build,
      "go",
      ["build", "-buildvcs=false", "-o", binary, "./cmd/sandrun-api"],
      env,
      launcherLifetime,
    );
    if (received) return stoppedStatus();
    if (buildStatus !== 0) return buildStatus;
    if (launcherAllowsPhase && !(await launcherAllowsPhase())) return 1;
    if (received) return stoppedStatus();
    const serverStatus = await run(
      Phase.Server,
      binary,
      [],
      env,
      launcherLifetime,
    );
    return received ? stoppedStatus() : serverStatus;
  } finally {
    await rm(buildDir, { recursive: true, force: true });
  }
}

const isMain = isWorkspaceCli(import.meta);
if (isMain && process.argv[2] === SuperviseFlag) {
  supervise(process.argv[3], process.argv[4], process.argv.slice(5));
} else if (isMain) {
  const handlers = installSignalHandlers();
  try {
    process.exitCode = await (process.argv[2] === CoordinateFlag
      ? coordinate()
      : main());
  } catch (error) {
    logFailure(error);
    process.exitCode = 1;
  } finally {
    for (const [signal, handler] of handlers) process.off(signal, handler);
  }
}
