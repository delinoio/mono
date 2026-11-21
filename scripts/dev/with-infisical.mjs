// SPDX-License-Identifier: Apache-2.0

// Development environment launcher. A package `dev` script runs its
// development command through this launcher:
//
//   node ../../scripts/dev/with-infisical.mjs --path=/<folder> -- <command> [args...]
//
// When the repository root has an Infisical project configuration
// (`.infisical.yaml`, or the legacy `.infisical.json`, which `infisical init`
// creates and Git ignores), the launcher runs
//
//   infisical run --env=dev --project-config-dir=<root> --path=<folder> -- <command> [args...]
//
// so that the command receives the variables of the `dev` environment in the
// package's own folder. `--env=dev` is fixed: a default environment or a
// Git-branch mapping in the local project configuration cannot select another
// environment. Without a project configuration, the launcher runs the command
// directly with the inherited environment, so a checkout without Infisical
// access (for example an external contributor's) keeps working. When the
// configuration exists, injection is required: a missing CLI, a missing login
// or a failed fetch stops startup and the command does not run.
//
// The Infisical CLI injects its values over variables that are already set.
// The launcher does not change that order. See
// docs/repository-environment-contract.md#local-development-injection.
//
// Process handling follows the server development launchers: the child runs
// in its own process group on POSIX, the launcher forwards SIGINT, SIGTERM,
// SIGQUIT and SIGHUP to it once, waits for it to exit and exits with its
// status. The Infisical CLI forwards every signal that it receives to its own
// child but does not start that child in a new process group, so without the
// separate group a terminal Ctrl-C would reach the development command twice
// (directly and through the CLI) and abort a graceful shutdown. Windows is
// excluded: there `detached` opens a separate console window instead of
// creating a process group.
//
// Child processes use explicit argument arrays and `shell: false`, so the
// command must be an executable on PATH. On Windows a `node_modules/.bin`
// shim is a `.cmd` file that `shell: false` cannot start; package scripts
// therefore run JavaScript tools as `node <package bin file>`.
//
// The launcher logs structured records on stderr. It never logs variable
// names or values.
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { constants } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// How the development command receives its environment.
export const InjectionMode = Object.freeze({
  // Through `infisical run` with the root project configuration.
  Infisical: "infisical",
  // Directly, with the inherited environment only.
  Direct: "direct",
});

// Project configuration file names, in the order in which the Infisical CLI
// reads them from one directory.
export const ProjectConfigFile = Object.freeze({
  Yaml: ".infisical.yaml",
  LegacyJson: ".infisical.json",
});

// The only Infisical environment that development commands read.
export const DevelopmentEnvironment = "dev";

const ForwardedSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  Quit: "SIGQUIT",
  HangUp: "SIGHUP",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];
const ownProcessGroup = process.platform !== "win32";

// Exit status for invalid launcher arguments, as for other usage errors.
const UsageExitCode = 2;

const repositoryRoot = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../..",
);

const log = (record) => console.error(JSON.stringify(record));

class UsageError extends Error {}

// A folder path is absolute, has no empty, `.` or `..` segment except the root
// itself, and contains only printable characters without whitespace.
function validFolder(path) {
  if (path === "/") return true;
  if (!path.startsWith("/") || /[\s\p{Cc}\\]/u.test(path)) return false;
  return path
    .slice(1)
    .split("/")
    .every((segment) => segment !== "" && segment !== "." && segment !== "..");
}

// Parses `--path=<folder> -- <command> [args...]`.
export function parseArguments(argv) {
  const separator = argv.indexOf("--");
  if (separator === -1) {
    throw new UsageError("expected `--` before the development command");
  }
  const options = argv.slice(0, separator);
  const command = argv.slice(separator + 1);
  if (command.length === 0) {
    throw new UsageError("expected a development command after `--`");
  }
  let folder = null;
  for (const option of options) {
    if (option.startsWith("--path=") && folder === null) {
      folder = option.slice("--path=".length);
    } else {
      throw new UsageError(
        `unexpected launcher argument ${JSON.stringify(option)}`,
      );
    }
  }
  if (folder === null) {
    throw new UsageError(
      "expected --path=<folder>, the package's Infisical folder",
    );
  }
  if (!validFolder(folder)) {
    throw new UsageError(
      `invalid Infisical folder ${JSON.stringify(folder)}; use an absolute path such as /accounts-api`,
    );
  }
  return { folder, command };
}

// Returns the project configuration file that the CLI would read from `root`,
// or null.
export function projectConfigFile(root) {
  return (
    Object.values(ProjectConfigFile).find((name) =>
      existsSync(join(root, name)),
    ) ?? null
  );
}

// Returns the executable and arguments for one launch.
export function launchPlan({ root, folder, command, configFile }) {
  if (configFile === null) {
    return {
      mode: InjectionMode.Direct,
      executable: command[0],
      args: command.slice(1),
    };
  }
  return {
    mode: InjectionMode.Infisical,
    executable: "infisical",
    args: [
      "run",
      `--env=${DevelopmentEnvironment}`,
      `--project-config-dir=${root}`,
      `--path=${folder}`,
      "--",
      ...command,
    ],
  };
}

let received = null;
let activeChild = null;

// Records the first stop signal and forwards every one to the active child.
// Between the launch decision and the spawn there is no child; main() checks
// `received` before it spawns.
function installSignalForwarding() {
  const handlers = Object.values(ForwardedSignal).map((signal) => {
    const handler = () => {
      received ??= signal;
      log({ event: "dev_env_signal", signal });
      activeChild?.kill(signal);
    };
    process.on(signal, handler);
    return [signal, handler];
  });
  return () => {
    for (const [signal, handler] of handlers) process.off(signal, handler);
  };
}

// Runs one child with inherited stdio and resolves with its exit status: its
// exit code, or the shell convention for a fatal signal.
function run(executable, args) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(executable, args, {
      stdio: "inherit",
      shell: false,
      detached: ownProcessGroup,
    });
    activeChild = child;
    child.once("error", (error) => {
      activeChild = null;
      reject(error);
    });
    child.once("close", (code, signal) => {
      activeChild = null;
      resolvePromise(code ?? (signal ? signalExitCode(signal) : 1));
    });
  });
}

async function main() {
  let parsed;
  try {
    parsed = parseArguments(process.argv.slice(2));
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    log({ event: "dev_env_usage_error", error: error.message });
    return UsageExitCode;
  }
  const configFile = projectConfigFile(repositoryRoot);
  const plan = launchPlan({ root: repositoryRoot, ...parsed, configFile });
  if (plan.mode === InjectionMode.Infisical) {
    log({
      event: "dev_env_infisical",
      environment: DevelopmentEnvironment,
      folder: parsed.folder,
      config: configFile,
    });
  } else {
    log({
      event: "dev_env_direct",
      reason: "no_infisical_project_config",
      folder: parsed.folder,
    });
  }
  // A signal before the launch was only recorded; the command must not start.
  if (received) return signalExitCode(received);
  try {
    const status = await run(plan.executable, plan.args);
    return received ? signalExitCode(received) : status;
  } catch (error) {
    if (plan.mode === InjectionMode.Infisical && error.code === "ENOENT") {
      log({
        event: "dev_env_failed",
        reason: "infisical_cli_missing",
        error:
          "The Infisical CLI is not on PATH. Install it, or remove the local project configuration to run without Infisical.",
      });
      return 1;
    }
    throw error;
  }
}

// The module also exports its pure helpers for tests; run only as a script.
if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const removeSignalForwarding = installSignalForwarding();
  try {
    process.exitCode = await main();
  } catch (error) {
    log({ event: "dev_env_failed", error: String(error) });
    process.exitCode = 1;
  } finally {
    removeSignalForwarding();
  }
}
