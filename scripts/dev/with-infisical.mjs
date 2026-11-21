// SPDX-License-Identifier: Apache-2.0

// Development environment launcher. A package `dev` script runs its
// development command through this launcher:
//
//   node ../../scripts/dev/with-infisical.mjs --path=/<folder> -- <command> [args...]
//
// When the repository root has an Infisical project configuration
// (`.infisical.yaml`, or the legacy `.infisical.json`, which `infisical init`
// creates and Git ignores), the launcher fetches the variables of the `dev`
// environment in the package's folder with
//
//   infisical export --env=dev --path=<folder> --format=json ...
//
// and starts the command with those variables added over the inherited
// environment. `--env=dev` is fixed: a default environment or a Git-branch
// mapping in the local project configuration cannot select another
// environment. Without a project configuration, the launcher starts the
// command directly with the inherited environment, so a checkout without
// Infisical access (for example an external contributor's) keeps working.
// When the configuration exists, injection is required: a missing CLI, a
// missing login or a failed fetch stops startup and the command does not run.
//
// Why `export` and not `infisical run`: `infisical run` keeps running as the
// parent of the command and forwards every signal that it receives to it. In a
// terminal, Ctrl-C reaches the whole foreground process group, so the command
// would receive SIGINT twice (directly and through the CLI), and a server
// development launcher aborts its graceful drain on the second signal. Moving
// the command into its own process group avoids the duplicate, but then
// Turborepo's forced stop of a task (SIGKILL to the task's process group)
// misses the command and leaves it running with its port. With `export`, the
// CLI exits before the command starts, so the command stays in the task's
// process group like a command that a package script starts directly.
//
// Signals: the launcher does not forward signals. The command is in the same
// process group and receives terminal and process-group signals itself. The
// launcher records SIGINT, SIGTERM, SIGQUIT and SIGHUP instead of exiting, so
// it waits for the command to finish its shutdown and exits with the
// command's status. A signal that arrives before the command starts (for
// example during the fetch) prevents the start. A signal sent only to the
// launcher's process ID does not reach the command; signal the process group.
//
// Child processes use explicit argument arrays and `shell: false`, so the
// command must be an executable on PATH. On Windows a `node_modules/.bin`
// shim is a `.cmd` file that `shell: false` cannot start; package scripts
// therefore run JavaScript tools as `node <package bin file>`.
//
// The launcher logs structured records on stderr. It never logs variable
// names or values, and the fetched values never leave the launcher's memory
// except as the command's environment.
import { spawn } from "node:child_process";
import { existsSync, realpathSync } from "node:fs";
import { constants } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// How the development command receives its environment.
export const InjectionMode = Object.freeze({
  // Through `infisical export` with the root project configuration.
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

// Reasons for which the launcher stops before the command starts.
export const FailureReason = Object.freeze({
  CliMissing: "infisical_cli_missing",
  FetchFailed: "infisical_fetch_failed",
  OutputInvalid: "infisical_output_invalid",
});

// Names that the launcher never takes from Infisical, so that a development
// folder cannot replace the process's own search path, home or terminal
// settings. The list matches the reserved names of `infisical run`.
const ReservedNames = new Set([
  "HOME",
  "PATH",
  "PS1",
  "PS2",
  "PWD",
  "EDITOR",
  "XAUTHORITY",
  "USER",
  "TERM",
  "TERMINFO",
  "SHELL",
  "MAIL",
]);
const ReservedPrefixes = ["XDG_", "LC_"];

// An environment variable name that a POSIX shell and Node.js both accept.
const VariableName = /^[A-Za-z_][A-Za-z0-9_]*$/;

// Upper bound of the CLI output that the launcher reads.
const MaxExportBytes = 16 * 1024 * 1024;

const StopSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  Quit: "SIGQUIT",
  HangUp: "SIGHUP",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];

// Exit status for invalid launcher arguments, as for other usage errors.
const UsageExitCode = 2;

const repositoryRoot = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../..",
);

const log = (record) => console.error(JSON.stringify(record));

class UsageError extends Error {}

class LaunchFailure extends Error {
  constructor(reason, message) {
    super(message);
    this.reason = reason;
  }
}

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
      `invalid Infisical folder ${JSON.stringify(folder)}; use an absolute path such as /servers/accounts-api`,
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

// Variables that configure the CLI's own logging. The servers use `LOG_LEVEL`
// for their logs, so a shell value such as `LOG_LEVEL=debug` must not make the
// CLI log debug records, and `LOG_DESTINATION=stdout` would mix its logs into
// the JSON output. The CLI reads `LOG_LEVEL` before it parses its flags, so the
// launcher removes these variables from the CLI's environment and also passes
// `--log-level`. It does not pass `--log-destination`: the CLI logs to
// standard error by default, and CLI versions before the flag was added (for
// example 0.43.110) reject it as an unknown flag.
const CliLoggingVariables = ["LOG_LEVEL", "LOG_DESTINATION", "LOG_FORMAT"];

// Environment of the fetch: the inherited environment without the CLI
// logging variables. The development command still receives them.
export function exportEnvironment(env) {
  const result = { ...env };
  for (const name of CliLoggingVariables) delete result[name];
  return result;
}

// Arguments of the fetch.
export function exportArguments(folder) {
  return [
    "export",
    `--env=${DevelopmentEnvironment}`,
    `--path=${folder}`,
    "--format=json",
    "--silent",
    "--log-level=warn",
  ];
}

// Converts the JSON output of `infisical export` (an array of objects with
// string `key` and `value` fields) into variables. Reserved names and names
// that are not valid variable names are skipped and only counted.
export function parseExport(output) {
  let entries;
  try {
    entries = JSON.parse(output);
  } catch {
    throw new LaunchFailure(
      FailureReason.OutputInvalid,
      "infisical export did not print JSON",
    );
  }
  if (!Array.isArray(entries)) {
    throw new LaunchFailure(
      FailureReason.OutputInvalid,
      "infisical export did not print a JSON array",
    );
  }
  const variables = {};
  let reserved = 0;
  let invalid = 0;
  for (const entry of entries) {
    if (
      entry === null ||
      typeof entry !== "object" ||
      typeof entry.key !== "string" ||
      typeof entry.value !== "string"
    ) {
      throw new LaunchFailure(
        FailureReason.OutputInvalid,
        "infisical export printed an entry without a string key and value",
      );
    }
    if (
      ReservedNames.has(entry.key) ||
      ReservedPrefixes.some((prefix) => entry.key.startsWith(prefix))
    ) {
      reserved += 1;
    } else if (!VariableName.test(entry.key) || entry.value.includes("\0")) {
      invalid += 1;
    } else {
      variables[entry.key] = entry.value;
    }
  }
  return { variables, reserved, invalid };
}

let received = null;

// Records the first stop signal. The handlers also keep the launcher alive,
// so that it waits for the command, which receives the same group signal.
function installSignalRecording() {
  const handlers = Object.values(StopSignal).map((signal) => {
    const handler = () => {
      received ??= signal;
      log({ event: "dev_env_signal", signal });
    };
    process.on(signal, handler);
    return [signal, handler];
  });
  return () => {
    for (const [signal, handler] of handlers) process.off(signal, handler);
  };
}

// Resolves with the exit status of a child: its exit code, or the shell
// convention for a fatal signal.
function exitStatus(code, signal) {
  return code ?? (signal ? signalExitCode(signal) : 1);
}

// Runs `infisical export` in the repository root, where the CLI finds the
// project configuration, and resolves with its standard output.
function fetchVariables(folder) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn("infisical", exportArguments(folder), {
      cwd: repositoryRoot,
      env: exportEnvironment(process.env),
      // No standard input: a missing login fails instead of waiting for an
      // interactive prompt. The CLI writes its errors to standard error.
      stdio: ["ignore", "pipe", "inherit"],
      shell: false,
    });
    const chunks = [];
    let size = 0;
    let overflow = false;
    child.stdout.on("data", (chunk) => {
      size += chunk.length;
      if (size > MaxExportBytes) {
        overflow = true;
        child.kill("SIGTERM");
      } else {
        chunks.push(chunk);
      }
    });
    child.once("error", (error) => {
      reject(
        error.code === "ENOENT"
          ? new LaunchFailure(
              FailureReason.CliMissing,
              "The Infisical CLI is not on PATH. Install it, or remove the local project configuration to run without Infisical.",
            )
          : error,
      );
    });
    child.once("close", (code, signal) => {
      if (overflow) {
        reject(
          new LaunchFailure(
            FailureReason.OutputInvalid,
            `infisical export printed more than ${MaxExportBytes} bytes`,
          ),
        );
      } else if (code !== 0) {
        reject(
          new LaunchFailure(
            FailureReason.FetchFailed,
            `infisical export exited with status ${exitStatus(code, signal)}; run \`infisical login\` or check access to the development project`,
          ),
        );
      } else {
        resolvePromise(Buffer.concat(chunks).toString("utf8"));
      }
    });
  });
}

// Runs the development command in the launcher's process group with
// inherited standard streams and resolves with its exit status.
function runCommand(command, env) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command[0], command.slice(1), {
      env,
      stdio: "inherit",
      shell: false,
    });
    child.once("error", (error) => {
      reject(error);
    });
    child.once("close", (code, signal) => {
      resolvePromise(exitStatus(code, signal));
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
  const { folder, command } = parsed;
  const configFile = projectConfigFile(repositoryRoot);
  const mode =
    configFile === null ? InjectionMode.Direct : InjectionMode.Infisical;
  let env = process.env;
  if (mode === InjectionMode.Direct) {
    log({
      event: "dev_env_direct",
      reason: "no_infisical_project_config",
      folder,
    });
  } else {
    let fetched;
    try {
      fetched = parseExport(await fetchVariables(folder));
    } catch (error) {
      if (received) return signalExitCode(received);
      if (!(error instanceof LaunchFailure)) throw error;
      log({
        event: "dev_env_failed",
        reason: error.reason,
        error: error.message,
      });
      return 1;
    }
    // Infisical values replace inherited values of the same name, as
    // `infisical run` does.
    env = { ...process.env, ...fetched.variables };
    log({
      event: "dev_env_infisical",
      environment: DevelopmentEnvironment,
      folder,
      config: configFile,
      variables: Object.keys(fetched.variables).length,
      reservedSkipped: fetched.reserved,
      invalidSkipped: fetched.invalid,
    });
  }
  // A signal before the start (for example during the fetch) was only
  // recorded; the command must not start after it.
  if (received) return signalExitCode(received);
  return runCommand(command, env);
}

// The module also exports its pure helpers for tests; run only as a script.
// Node.js loads the main module from its real path, so `import.meta.url` has
// no symbolic links while `process.argv[1]` keeps the path as given. Compare
// real paths: a checkout under a symbolic link (on macOS the temporary
// directory under `/var` is one) would otherwise exit 0 without starting the
// command.
if (
  process.argv[1] &&
  realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const removeSignalRecording = installSignalRecording();
  try {
    process.exitCode = await main();
  } catch (error) {
    log({ event: "dev_env_failed", error: String(error) });
    process.exitCode = 1;
  } finally {
    removeSignalRecording();
  }
}
