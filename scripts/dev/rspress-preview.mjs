// SPDX-License-Identifier: Apache-2.0

// Rspress preview launcher. The `preview` script of an Rspress app runs
//
//   node ../../scripts/dev/rspress-preview.mjs [args...]
//
// and the launcher starts `rspress preview [args...]` from the package's
// working directory with the `PORT` and `HOST` variables removed.
//
// Workaround: `rspress preview` takes its port and host from `process.env.PORT`
// and `process.env.HOST` before the `--port` and `--host` flags and before
// `builderConfig.server` in `rspress.config.ts` (verified in `@rspress/core`
// 2.1.3, `dist/node/serve.js`). A `PORT` or `HOST` that the developer's shell
// exports therefore moves the preview server away from the fixed loopback port
// that the app's configuration selects, and no configuration can prevent it.
// Removing the two variables for the preview process only restores the
// configuration's fixed `127.0.0.1` address and `strictPort`. Remove this
// launcher and return the scripts to `rspress preview` when Rspress prefers
// its flags and configuration over `PORT` and `HOST`, or ignores them in
// `preview`. The `dev:app` script needs no launcher: `rspress dev` does not
// read these variables, and portless passes its own `--port` and `--host`
// flags.
//
// The launcher finds the Rspress CLI from the package directory
// (`@rspress/core` `package.json` `bin.rspress`) and starts it with the
// running Node.js executable, `shell: false` and the inherited standard
// streams. A `node_modules/.bin` shim is a `.cmd` file on Windows that
// `shell: false` cannot start, and `env -u PORT` does not exist there either.
//
// Signals: the launcher does not forward signals. The Rspress process is in the
// same process group and receives terminal and process-group signals itself,
// exactly once. The launcher records SIGINT, SIGTERM, SIGQUIT and SIGHUP
// instead of exiting, waits for the process and exits with its status (its exit
// code, or `128 + <signal number>` when a signal ended it). A signal sent only
// to the launcher's process ID does not reach Rspress; signal the process
// group.
//
// The launcher logs structured records on standard error: `rspress_preview_env`
// when it removes a variable (names only) and `rspress_preview_failed` when it
// cannot find or start the CLI.
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { constants } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// The package whose CLI the launcher starts and the bin entry it reads.
export const RspressPackage = "@rspress/core";
export const RspressBinName = "rspress";

// Variables that `rspress preview` reads before its flags and configuration.
export const PreviewOverrideVariables = Object.freeze(["PORT", "HOST"]);

// Reasons for which the launcher stops before Rspress runs.
export const FailureReason = Object.freeze({
  CliNotFound: "rspress_cli_not_found",
  SpawnFailed: "rspress_spawn_failed",
});

const StopSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  Quit: "SIGQUIT",
  HangUp: "SIGHUP",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];

const log = (record) => console.error(JSON.stringify(record));

class LaunchFailure extends Error {
  constructor(reason, message) {
    super(message);
    this.reason = reason;
  }
}

// Returns a copy of `env` without the variables that override the preview
// address, and the names that it removed. Windows variable names are case
// insensitive, and the copy of `process.env` keeps the original case, so
// `Port` is removed there too.
export function previewEnvironment(env, platform = process.platform) {
  const result = { ...env };
  const removed = [];
  for (const key of Object.keys(result)) {
    const name = platform === "win32" ? key.toUpperCase() : key;
    if (PreviewOverrideVariables.includes(name)) {
      delete result[key];
      removed.push(name);
    }
  }
  return { env: result, removed };
}

// Returns the absolute path of the Rspress CLI file that the package in `cwd`
// resolves.
export function resolveRspressBin(cwd) {
  let manifestPath;
  try {
    manifestPath = createRequire(resolve(cwd, "package.json")).resolve(
      `${RspressPackage}/package.json`,
    );
  } catch {
    throw new LaunchFailure(
      FailureReason.CliNotFound,
      `${RspressPackage} is not installed for ${cwd}; run \`pnpm install\``,
    );
  }
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  const bin =
    typeof manifest.bin === "string"
      ? manifest.bin
      : manifest.bin?.[RspressBinName];
  if (typeof bin !== "string" || bin === "") {
    throw new LaunchFailure(
      FailureReason.CliNotFound,
      `${RspressPackage} declares no \`${RspressBinName}\` bin file`,
    );
  }
  return resolve(dirname(manifestPath), bin);
}

// Resolves with the exit status of a child: its exit code, or the shell
// convention for a fatal signal.
function exitStatus(code, signal) {
  return code ?? (signal ? signalExitCode(signal) : 1);
}

// Records stop signals without exiting, so that the launcher waits for Rspress,
// which receives the same terminal or process-group signal.
function installSignalRecording() {
  const handlers = Object.values(StopSignal).map((signal) => {
    const handler = () => {
      log({ event: "rspress_preview_signal", signal });
    };
    process.on(signal, handler);
    return [signal, handler];
  });
  return () => {
    for (const [signal, handler] of handlers) process.off(signal, handler);
  };
}

function runPreview(bin, args, env) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(process.execPath, [bin, "preview", ...args], {
      env,
      stdio: "inherit",
      shell: false,
    });
    child.once("error", (error) => {
      reject(
        new LaunchFailure(
          FailureReason.SpawnFailed,
          `cannot start ${bin}: ${error.message}`,
        ),
      );
    });
    child.once("close", (code, signal) => {
      resolvePromise(exitStatus(code, signal));
    });
  });
}

async function main() {
  const bin = resolveRspressBin(process.cwd());
  const { env, removed } = previewEnvironment(process.env);
  if (removed.length > 0) {
    log({
      event: "rspress_preview_env",
      removed,
      reason: "rspress_preview_reads_port_and_host_before_config",
    });
  }
  return runPreview(bin, process.argv.slice(2), env);
}

// The module also exports its pure helpers for tests; run only as a script.
if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const removeSignalRecording = installSignalRecording();
  try {
    process.exitCode = await main();
  } catch (error) {
    log({
      event: "rspress_preview_failed",
      reason: error instanceof LaunchFailure ? error.reason : undefined,
      error: error instanceof Error ? error.message : String(error),
    });
    process.exitCode = 1;
  } finally {
    removeSignalRecording();
  }
}
