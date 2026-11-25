// SPDX-License-Identifier: FSL-1.1-ALv2

// Development launcher for accounts-api (`pnpm --filter accounts-api dev`).
//
// It replaces `go run ./cmd/accounts-api`. `go run` forwards only SIGINT and
// SIGQUIT to the compiled server and does not tie the server's lifetime to its
// own, so a SIGTERM sent to the launcher process (`kill`, `timeout`, a
// supervisor, pnpm or Turbo) ended `go run` and left the server listening
// without a graceful shutdown. This launcher builds the server into a
// temporary directory, runs the binary as its child, forwards SIGINT,
// SIGTERM and SIGQUIT to the active phase, waits for it to stop and removes the
// build output afterwards. Each POSIX phase runs in its own process group so a
// process-group signal (a terminal Ctrl-C or Ctrl-\) reaches it exactly once,
// through the launcher's forward. SIGQUIT makes the Go server print its
// goroutine dump and exit; the launcher then exits with status 131.
//
// SIGHUP is not handled: it ends the launcher, and so does SIGKILL. On POSIX
// the build and the server run under a supervisor that stops them when the
// launcher dies (see "Forced-stop protection" below), so a forced stop of the
// task's process group, such as Turborepo's, leaves no server behind and its
// port becomes free again. The temporary build directory is not removed on
// that path.
//
// The server needs Postgres and the billing configuration. When DATABASE_URL
// is unset, empty or only ASCII white space, the launcher prints an
// `accounts_api_dev_skipped` record and exits with status 0 instead of
// failing, so the root `pnpm dev` keeps running the other development servers
// on machines without a database or Infisical. A value that consists only of
// Unicode white space, such as U+00A0, is not unset: the launcher passes it on
// and the server rejects it. When CORS_ORIGINS is unset, empty or only ASCII
// white space, it allows the web app's development origins in addition to the
// production origin: http://127.0.0.1:13100 (the fixed port of `dev:app` and
// `PORTLESS=0`) and https://accounts.delino.localhost and
// http://accounts.delino.localhost (the portless URL with the default proxy and
// with an unprivileged proxy). When WEB_ORIGIN is unset, empty or only ASCII
// white space, invitation links point to https://accounts.delino.localhost. A
// proxy on a custom PORTLESS_PORT adds that port to the URL; set CORS_ORIGINS
// and WEB_ORIGIN to it there. On Windows, where environment variable names are
// case-insensitive, an existing `cors_origins` or `Web_Origin` counts as the
// setting: the launcher keeps a nonempty value and writes a default into that
// same entry, so the server never receives a second spelling. On POSIX only the
// exact uppercase names count and differently cased names stay distinct
// variables.
//
// Child processes use explicit argument arrays and `shell: false` according to
// the repository environment contract. Other configuration, standard streams
// and the working directory reach the server unchanged.
import { spawn } from "node:child_process";
import { closeSync, openSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import net from "node:net";
import { constants, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Signals that stop the launcher. Each one is forwarded to the active phase.
const ForwardedSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  // Without a handler, Node would end the launcher on SIGQUIT, and the
  // supervisor would then stop the server with SIGTERM instead of letting it
  // print its goroutine dump.
  Quit: "SIGQUIT",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];
const DevelopmentWebOrigins = Object.freeze([
  "http://127.0.0.1:13100",
  "https://accounts.delino.localhost",
  "http://accounts.delino.localhost",
]);
const ProductionWebOrigin = "https://accounts.delino.io";
const DevelopmentInvitationOrigin = "https://accounts.delino.localhost";

// The server removes only ASCII white space (space, tab, LF, VT, FF, CR) from
// configuration values, so the launcher must use the same rule to decide that a
// value is unset. JavaScript's trim() and \s also remove Unicode white space
// such as U+00A0 and U+3000, which would hide a value the server rejects.
const AsciiWhiteSpaceOnly = /^[\t\n\v\f\r ]*$/;
const isUnset = (value) =>
  value === undefined || AsciiWhiteSpaceOnly.test(value);

// Children run in their own process group (and session) on POSIX, under the
// supervisor described below. A terminal
// Ctrl-C signals the whole foreground group; with the child in that group it
// would receive SIGINT directly and then again from the launcher's forward. The
// server releases its signal registration on the first signal (a second signal
// terminates it immediately), so the duplicate would abort the graceful drain.
// Detached, only the launcher receives the group signal and forwards it once.
// Windows is excluded: there `detached` opens a separate console window for the
// child instead of creating a process group, and the launcher's Windows
// behavior is unchanged. Remove the platform condition only together with a
// Windows-specific signal design.
const ownProcessGroup = process.platform !== "win32";

// Finds the entries of `name` (an uppercase variable name) in a plain copy of
// the environment. A plain object compares keys exactly, but Windows treats
// names that differ only by case as one variable and Node then keeps only one
// spelling for the child, so a default added under the uppercase name could
// replace an explicit lowercase value. POSIX keeps differently cased names
// distinct.
function envAliases(env, name, platform) {
  const matches =
    platform === "win32"
      ? (key) => key.toUpperCase() === name
      : (key) => key === name;
  // Sorted like the entries that Node sorts before it builds the Windows
  // environment block, so the choice below is the same on every run.
  return Object.keys(env).filter(matches).sort();
}

// Applies `value` to `name` unless the environment already has a nonempty
// value, and keeps exactly one entry for the setting. Mutates `env`.
function applyEnvDefault(env, name, value, platform) {
  const aliases = envAliases(env, name, platform);
  const key =
    aliases.find((alias) => !isUnset(env[alias])) ?? aliases[0] ?? name;
  for (const alias of aliases) if (alias !== key) delete env[alias];
  if (isUnset(env[key])) env[key] = value;
}

// Builds the environment of the build and the server: a copy of `baseEnv`
// with the development defaults for the settings that are unset. Exported for
// the launcher tests, which run it with both platform rules.
export function resolveChildEnv(baseEnv, platform = process.platform) {
  const env = { ...baseEnv };
  applyEnvDefault(
    env,
    "CORS_ORIGINS",
    [ProductionWebOrigin, ...DevelopmentWebOrigins].join(","),
    platform,
  );
  applyEnvDefault(env, "WEB_ORIGIN", DevelopmentInvitationOrigin, platform);
  return env;
}

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const binaryName =
  process.platform === "win32" ? "accounts-api.exe" : "accounts-api";

let received = null;
let forwardSignal = null;
function installSignalHandlers() {
  return Object.values(ForwardedSignal).map((signal) => {
    const handler = () => {
      received ??= signal;
      console.error(
        JSON.stringify({ event: "accounts_api_dev_signal", signal }),
      );
      // Between phases there is no child; the phase boundary checks `received`.
      forwardSignal?.(signal);
    };
    process.on(signal, handler);
    return [signal, handler];
  });
}

// Forced-stop protection (POSIX). A child that runs in its own process group
// is outside the group that a task runner kills, and SIGKILL of the launcher
// cannot be forwarded, so a build or server would survive it and keep its
// port. The launcher therefore starts each phase through a supervisor: this
// file started again with `--supervise` in a process group of its own, with a
// private channel (file descriptor 3) to the launcher. The supervisor runs the
// build or server as its child in the same group. The build supervisor reads
// stop requests from the channel and signals its whole group. The server
// supervisor relays OS signals to the server once per delivery. When the
// launcher dies, the kernel closes the channel. The supervisor then sends
// SIGTERM to its group so a server can drain. A group stop ends with SIGKILL
// when the child exits or the grace period expires. This ends surviving
// descendants. The launcher's end of the channel stays in the launcher. The
// supervisor's child gets /dev/null as descriptor 3, so the pipe does not
// reach the build, the server or their descendants.
// Windows starts the child directly (`ownProcessGroup` is false there).
const SuperviseFlag = "--supervise";
const StopGraceMs = 5000;
const Phase = Object.freeze({ Build: "build", Server: "server" });
const BuildMessage = Object.freeze({ Exited: "exited", Finish: "finish" });

function supervise(phase, command, args) {
  const isBuild = phase === Phase.Build;
  let stoppingGroup = false;
  let child = null;
  let childStatus = null;
  // The supervisor leads its group (the launcher starts it detached), so the
  // group id is its own pid and the child and its descendants are members.
  const stopGroup = (signal) => {
    try {
      process.kill(-process.pid, signal);
    } catch {
      child?.kill(signal);
    }
  };
  for (const signal of Object.values(ForwardedSignal)) {
    process.on(signal, () => {
      // A group signal reaches the supervisor too. Only the server supervisor
      // relays OS signals, and only before a group stop.
      if (!isBuild && !stoppingGroup) child?.kill(signal);
    });
  }
  const finish = () => {
    if (stoppingGroup) stopGroup("SIGKILL");
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
    console.error(JSON.stringify({ event: "accounts_api_dev_launcher_gone" }));
    requestGroupStop("SIGTERM");
  };
  if (isBuild) {
    process.on("disconnect", onLauncherGone);
    process.on("message", (message) => {
      if (message === BuildMessage.Finish) {
        finish();
      } else {
        requestGroupStop(message);
      }
    });
  } else {
    const pipe = new net.Socket({ fd: 3, readable: true, writable: false });
    pipe.on("error", onLauncherGone);
    pipe.on("close", onLauncherGone);
    pipe.resume();
  }

  // Descriptor 3 is the pipe here. Spawning with an "ignore" entry would leave
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
    console.error(
      JSON.stringify({
        event: "accounts_api_dev_failed",
        error: String(error),
      }),
    );
    process.exit(1);
  });
  child.once("close", (code, signal) => {
    childStatus = code ?? (signal ? signalExitCode(signal) : 1);
    if (isBuild && !stoppingGroup) {
      // Wait for the launcher to acknowledge the exit. A stop it has already
      // forwarded arrives before that acknowledgment on the same channel.
      process.send(BuildMessage.Exited, (error) => {
        if (error) onLauncherGone();
      });
    } else {
      finish();
    }
  });
}

// Runs one child with inherited stdio and resolves with its exit status once
// it has exited: its exit code, or the shell convention for a fatal signal.
function run(phase, command, args, env) {
  return new Promise((resolvePromise, reject) => {
    // On POSIX the child runs under a supervisor in its own process group (see
    // the forced-stop protection above); the supervisor exits with the child's
    // status, or the shell convention for a fatal signal.
    const superviseBuild = ownProcessGroup && phase === Phase.Build;
    const child = ownProcessGroup
      ? spawn(
          process.execPath,
          [
            fileURLToPath(import.meta.url),
            SuperviseFlag,
            phase,
            command,
            ...args,
          ],
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
    // The server supervisor uses this pipe only to detect launcher death.
    const lifetimePipe = child.stdio[3];
    lifetimePipe?.on("error", () => {});
    if (superviseBuild) {
      // A signal can arrive as the build supervisor closes. In that case the
      // launcher has already recorded the signal and will skip the server.
      const send = (message) => child.send(message, () => {});
      child.once("message", (message) => {
        if (message === BuildMessage.Exited) send(BuildMessage.Finish);
      });
      forwardSignal = send;
    } else {
      forwardSignal = (signal) => child.kill(signal);
    }
    child.once("error", (error) => {
      forwardSignal = null;
      reject(error);
    });
    child.once("close", (code, signal) => {
      forwardSignal = null;
      lifetimePipe?.destroy();
      resolvePromise(code ?? (signal ? signalExitCode(signal) : 1));
    });
  });
}

// A stopped launcher reports the shell convention for the received signal.
const stoppedStatus = () => signalExitCode(received);

async function main() {
  // An empty or ASCII-white-space-only value counts as unset, as it does for
  // the server. Unicode white space does not, so the server reports it.
  if (isUnset(process.env.DATABASE_URL)) {
    console.error(
      JSON.stringify({
        event: "accounts_api_dev_skipped",
        reason: "DATABASE_URL is not set",
      }),
    );
    return 0;
  }
  const env = resolveChildEnv(process.env);
  const buildDir = await mkdtemp(join(tmpdir(), "accounts-api-dev-"));
  try {
    // A signal during the `mkdtemp` await above found no child to forward to
    // and was only recorded; the build must not start after it.
    if (received) return stoppedStatus();
    const binary = join(buildDir, binaryName);
    // -buildvcs=false matches the `build` script: in a linked Git worktree Go's
    // VCS discovery can fail with "error obtaining VCS status". Remove it
    // together with the `build` script flag as documented in
    // docs/servers-accounts-api-foundation.md.
    const buildStatus = await run(
      Phase.Build,
      "go",
      ["build", "-buildvcs=false", "-o", binary, "./cmd/accounts-api"],
      env,
    );
    // A signal during the build stops the launcher even when the build still
    // finished: the server must never start after a termination request.
    if (received) return stoppedStatus();
    if (buildStatus !== 0) return buildStatus;

    const serverStatus = await run(Phase.Server, binary, [], env);
    return received ? stoppedStatus() : serverStatus;
  } finally {
    // Reached only after the last child has exited, so nothing uses the binary.
    await rm(buildDir, { recursive: true, force: true });
  }
}

// Run only when started as the entry point, so the launcher tests can import
// the helpers above without starting a build. import.meta.main needs Node.js
// 24.2 or later, as the launchers in scripts/dev do.
if (import.meta.main && process.argv[2] === SuperviseFlag) {
  supervise(process.argv[3], process.argv[4], process.argv.slice(5));
} else if (import.meta.main) {
  const handlers = installSignalHandlers();
  try {
    process.exitCode = await main();
  } catch (error) {
    console.error(
      JSON.stringify({
        event: "accounts_api_dev_failed",
        error: String(error),
      }),
    );
    process.exitCode = 1;
  } finally {
    for (const [signal, handler] of handlers) process.off(signal, handler);
  }
}
