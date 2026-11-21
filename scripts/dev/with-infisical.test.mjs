// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  DevelopmentEnvironment,
  exportArguments,
  exportEnvironment,
  FailureReason,
  parseArguments,
  parseExport,
  ProjectConfigFile,
  projectConfigFile,
} from "./with-infisical.mjs";

const launcherSource = fileURLToPath(
  new URL("./with-infisical.mjs", import.meta.url),
);
const posixOnly = {
  skip:
    process.platform === "win32" &&
    "process groups and the fake CLI shebang are POSIX-only",
};

// A value that must never appear in launcher output.
const secretValue = "s3cr3t-value-that-must-not-be-logged";

// A fake `infisical` CLI. For `export` it records its arguments and working
// directory, optionally waits, and then prints FAKE_INFISICAL_OUTPUT on
// standard output and exits with FAKE_INFISICAL_EXIT. The file has no
// extension, so Node.js runs it as CommonJS.
const fakeInfisical = `#!/usr/bin/env node
const { writeFileSync } = require("node:fs");
writeFileSync(
  process.env.FAKE_INFISICAL_ARGS,
  JSON.stringify({
    args: process.argv.slice(2),
    cwd: process.cwd(),
    LOG_LEVEL: process.env.LOG_LEVEL ?? null,
  }),
);
const finish = () => {
  process.stdout.write(process.env.FAKE_INFISICAL_OUTPUT ?? "[]");
  process.exitCode = Number(process.env.FAKE_INFISICAL_EXIT ?? "0");
};
setTimeout(finish, Number(process.env.FAKE_INFISICAL_DELAY_MS ?? "0"));
`;

// The development command. It records its process ID, arguments and selected
// variables, prints a ready line, and on SIGINT records the signal and exits
// shortly afterwards, so that a duplicate delivery is also recorded.
const devCommand = `import { appendFileSync, writeFileSync } from "node:fs";
const out = process.env.DEV_COMMAND_OUT;
writeFileSync(out + ".start", JSON.stringify({
  pid: process.pid,
  args: process.argv.slice(2),
  DEV_VALUE: process.env.DEV_VALUE ?? null,
  SHELL_ONLY: process.env.SHELL_ONLY ?? null,
  HOME: process.env.HOME ?? null,
  LOG_LEVEL: process.env.LOG_LEVEL ?? null,
}));
process.on("SIGINT", () => {
  appendFileSync(out + ".signals", "SIGINT\\n");
  setTimeout(() => process.exit(0), 300);
});
if (process.argv[3] === "wait") {
  console.log("ready");
  setInterval(() => {}, 1000);
} else {
  process.exit(Number(process.argv[2] ?? "0"));
}
`;

// Builds a repository-like directory with the launcher at
// `scripts/dev/with-infisical.mjs`, so that the launcher resolves the
// fixture as its repository root.
function fixture(t, { config = null, cli = true, env = {} } = {}) {
  const root = mkdtempSync(join(tmpdir(), "with-infisical-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "scripts", "dev"), { recursive: true });
  const launcher = join(root, "scripts", "dev", "with-infisical.mjs");
  copyFileSync(launcherSource, launcher);
  if (config) writeFileSync(join(root, config), "version: 1\n");
  const bin = join(root, "bin");
  mkdirSync(bin);
  if (cli) {
    writeFileSync(join(bin, "infisical"), fakeInfisical);
    chmodSync(join(bin, "infisical"), 0o755);
  }
  // The command runs from a package directory, as a package script does.
  const packageDir = join(root, "apps", "example");
  mkdirSync(packageDir, { recursive: true });
  writeFileSync(join(packageDir, "dev-command.mjs"), devCommand);
  const out = join(root, "out");
  return {
    root,
    packageDir,
    launcher,
    out,
    infisicalArgs: join(root, "infisical-args.json"),
    env: {
      // Only the fake CLI directory and Node.js: a real `infisical` on the
      // host PATH must not be found.
      PATH: [bin, dirname(process.execPath)].join(delimiter),
      HOME: root,
      FAKE_INFISICAL_ARGS: join(root, "infisical-args.json"),
      DEV_COMMAND_OUT: out,
      ...env,
    },
  };
}

// Starts the launcher in its own process group, as a terminal or Turborepo
// starts a job, and collects its output.
function startLauncher(f, args) {
  const child = spawn(process.execPath, [f.launcher, ...args], {
    cwd: f.packageDir,
    env: f.env,
    stdio: ["ignore", "pipe", "pipe"],
    shell: false,
    detached: true,
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => (stdout += chunk));
  child.stderr.on("data", (chunk) => (stderr += chunk));
  const exited = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) =>
      resolve({ code, signal, stdout, stderr }),
    );
  });
  return { child, exited, output: () => stdout };
}

const records = (stderr) =>
  stderr
    .split("\n")
    .filter((line) => line.startsWith("{"))
    .map((line) => JSON.parse(line));

const commandArgs = (exitCode, ...rest) => [
  "--",
  process.execPath,
  "dev-command.mjs",
  String(exitCode),
  ...rest,
];

const started = (f) => JSON.parse(readFileSync(`${f.out}.start`, "utf8"));

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function waitFor(predicate, what) {
  const deadline = Date.now() + 10_000;
  while (!predicate()) {
    if (Date.now() > deadline) assert.fail(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

const exported = (entries) => JSON.stringify(entries);

test("parseArguments accepts one folder and a command", () => {
  assert.deepEqual(
    parseArguments([
      "--path=/servers/accounts-api",
      "--",
      "node",
      "dev.mjs",
      "--x",
    ]),
    { folder: "/servers/accounts-api", command: ["node", "dev.mjs", "--x"] },
  );
  assert.deepEqual(parseArguments(["--path=/", "--", "node"]), {
    folder: "/",
    command: ["node"],
  });
  assert.deepEqual(parseArguments(["--path=/a/b-c_d.e", "--", "node"]), {
    folder: "/a/b-c_d.e",
    command: ["node"],
  });
  // Arguments after `--`, including a second `--`, belong to the command.
  assert.deepEqual(
    parseArguments(["--path=/a", "--", "x", "--", "--path=/b"]),
    { folder: "/a", command: ["x", "--", "--path=/b"] },
  );
});

// Every package `dev` script that uses the launcher reads the Infisical folder
// equal to its package directory, so the folder tree mirrors the repository
// layout. See docs/repository-environment-contract.md#local-development-injection.
test("each launcher folder is the package directory", () => {
  const repositoryRoot = fileURLToPath(new URL("../..", import.meta.url));
  const launcherPrefix = "node ../../scripts/dev/with-infisical.mjs ";
  const found = [];
  for (const parent of ["apps", "servers", "packages"]) {
    if (!existsSync(join(repositoryRoot, parent))) continue;
    for (const entry of readdirSync(join(repositoryRoot, parent), {
      withFileTypes: true,
    })) {
      const manifestPath = join(
        repositoryRoot,
        parent,
        entry.name,
        "package.json",
      );
      if (!entry.isDirectory() || !existsSync(manifestPath)) continue;
      const dev = JSON.parse(readFileSync(manifestPath, "utf8")).scripts?.dev;
      if (typeof dev !== "string" || !dev.startsWith(launcherPrefix)) continue;
      const directory = `${parent}/${entry.name}`;
      const { folder } = parseArguments(
        dev.slice(launcherPrefix.length).split(" "),
      );
      assert.equal(folder, `/${directory}`, `${directory} dev script`);
      found.push(directory);
    }
  }
  assert.deepEqual(found.sort(), [
    "apps/delino-accounts",
    "apps/ownsand",
    "servers/accounts-api",
    "servers/ownsand-api",
  ]);
});

test("parseArguments rejects invalid launcher arguments", async (t) => {
  for (const argv of [
    ["--path=/a", "node"],
    ["--path=/a", "--"],
    ["--", "node"],
    ["--path=/a", "--path=/b", "--", "node"],
    ["--env=prod", "--path=/a", "--", "node"],
    ["--path=accounts-api", "--", "node"],
    ["--path=", "--", "node"],
    ["--path=/a/", "--", "node"],
    ["--path=//a", "--", "node"],
    ["--path=/a/../b", "--", "node"],
    ["--path=/./a", "--", "node"],
    ["--path=/a b", "--", "node"],
    ["--path=/a\\b", "--", "node"],
    ["--path=/a\nb", "--", "node"],
  ]) {
    await t.test(JSON.stringify(argv), () => {
      assert.throws(() => parseArguments(argv));
    });
  }
});

test("projectConfigFile prefers .infisical.yaml like the CLI", async (t) => {
  for (const [files, expected] of [
    [[], null],
    [[ProjectConfigFile.Yaml], ProjectConfigFile.Yaml],
    [[ProjectConfigFile.LegacyJson], ProjectConfigFile.LegacyJson],
    [
      [ProjectConfigFile.LegacyJson, ProjectConfigFile.Yaml],
      ProjectConfigFile.Yaml,
    ],
    // The CLI does not read `.infisical.yml`.
    [[".infisical.yml"], null],
  ]) {
    await t.test(JSON.stringify(files), (t) => {
      const dir = mkdtempSync(join(tmpdir(), "with-infisical-config-"));
      t.after(() => rmSync(dir, { recursive: true, force: true }));
      for (const file of files) writeFileSync(join(dir, file), "");
      assert.equal(projectConfigFile(dir), expected);
    });
  }
});

test("exportArguments pins the dev environment, the folder and logging", () => {
  assert.equal(DevelopmentEnvironment, "dev");
  assert.deepEqual(exportArguments("/api"), [
    "export",
    "--env=dev",
    "--path=/api",
    "--format=json",
    "--silent",
    "--log-level=warn",
    "--log-destination=stderr",
  ]);
});

test("exportEnvironment removes only the CLI logging variables", () => {
  assert.deepEqual(
    exportEnvironment({
      LOG_LEVEL: "debug",
      LOG_DESTINATION: "stdout",
      LOG_FORMAT: "json",
      HOME: "/home/user",
      DBUS_SESSION_BUS_ADDRESS: "unix:path=/run/bus",
    }),
    { HOME: "/home/user", DBUS_SESSION_BUS_ADDRESS: "unix:path=/run/bus" },
  );
});

test("parseExport keeps valid names and skips reserved and invalid ones", () => {
  assert.deepEqual(
    parseExport(
      exported([
        { key: "DATABASE_URL", value: "postgres://x", type: "shared" },
        { key: "EMPTY", value: "" },
        { key: "PATH", value: "/evil" },
        { key: "HOME", value: "/evil" },
        { key: "XDG_CONFIG_HOME", value: "/evil" },
        { key: "LC_ALL", value: "C" },
        { key: "1BAD", value: "x" },
        { key: "BAD-NAME", value: "x" },
        { key: "BAD=NAME", value: "x" },
        { key: "NUL_VALUE", value: "a\0b" },
      ]),
    ),
    {
      variables: { DATABASE_URL: "postgres://x", EMPTY: "" },
      reserved: 4,
      invalid: 4,
    },
  );
  assert.deepEqual(parseExport("[]"), {
    variables: {},
    reserved: 0,
    invalid: 0,
  });
});

test("parseExport rejects output that is not the export array", async (t) => {
  for (const output of [
    "",
    "KEY=value",
    "{}",
    "null",
    exported([{ key: "A" }]),
    exported([{ key: 1, value: "x" }]),
    exported(["A=b"]),
    exported([null]),
  ]) {
    await t.test(JSON.stringify(output), () => {
      assert.throws(
        () => parseExport(output),
        (error) => error.reason === FailureReason.OutputInvalid,
      );
    });
  }
});

test(
  "without a project configuration the command runs directly",
  posixOnly,
  async (t) => {
    const f = fixture(t);
    const result = await startLauncher(f, ["--path=/api", ...commandArgs(7)])
      .exited;
    assert.equal(result.code, 7, result.stderr);
    assert.equal(existsSync(f.infisicalArgs), false, "the CLI must not run");
    assert.deepEqual(started(f).args, ["7"]);
    assert.deepEqual(records(result.stderr), [
      {
        event: "dev_env_direct",
        reason: "no_infisical_project_config",
        folder: "/api",
      },
    ]);
  },
);

test(
  "with a project configuration the command receives the exported variables",
  posixOnly,
  async (t) => {
    for (const config of Object.values(ProjectConfigFile)) {
      await t.test(config, async (t) => {
        const f = fixture(t, {
          config,
          env: {
            DEV_VALUE: "from-shell",
            SHELL_ONLY: "kept",
            LOG_LEVEL: "debug",
            FAKE_INFISICAL_OUTPUT: exported([
              { key: "DEV_VALUE", value: secretValue },
              { key: "HOME", value: "/from-infisical" },
              { key: "not-a-name", value: "x" },
            ]),
          },
        });
        const result = await startLauncher(f, [
          "--path=/servers/accounts-api",
          ...commandArgs(3),
        ]).exited;
        assert.equal(result.code, 3, result.stderr);
        // The CLI runs in the repository root, where it finds the project
        // configuration, with the pinned arguments.
        // The CLI does not see the servers' LOG_LEVEL; the command does.
        assert.deepEqual(JSON.parse(readFileSync(f.infisicalArgs, "utf8")), {
          args: exportArguments("/servers/accounts-api"),
          cwd: f.root,
          LOG_LEVEL: null,
        });
        const start = started(f);
        assert.deepEqual(start.args, ["3"]);
        // Infisical values replace shell values; other shell values stay, and
        // reserved names keep their inherited values.
        assert.equal(start.DEV_VALUE, secretValue);
        assert.equal(start.SHELL_ONLY, "kept");
        assert.equal(start.HOME, f.root);
        assert.equal(start.LOG_LEVEL, "debug");
        assert.deepEqual(records(result.stderr), [
          {
            event: "dev_env_infisical",
            environment: "dev",
            folder: "/servers/accounts-api",
            config,
            variables: 1,
            reservedSkipped: 1,
            invalidSkipped: 1,
          },
        ]);
        assert.equal(result.stderr.includes(secretValue), false);
        assert.equal(result.stdout.includes(secretValue), false);
      });
    }
  },
);

test(
  "a configured checkout stops before the command when the fetch fails",
  posixOnly,
  async (t) => {
    for (const [name, options, reason] of [
      ["missing CLI", { cli: false }, FailureReason.CliMissing],
      [
        "failed export",
        { env: { FAKE_INFISICAL_EXIT: "1" } },
        FailureReason.FetchFailed,
      ],
      [
        "invalid output",
        { env: { FAKE_INFISICAL_OUTPUT: "KEY=value" } },
        FailureReason.OutputInvalid,
      ],
    ]) {
      await t.test(name, async (t) => {
        const f = fixture(t, { config: ProjectConfigFile.Yaml, ...options });
        const result = await startLauncher(f, [
          "--path=/api",
          ...commandArgs(0),
        ]).exited;
        assert.equal(result.code, 1, result.stderr);
        assert.equal(existsSync(`${f.out}.start`), false);
        const failure = records(result.stderr).find(
          (record) => record.event === "dev_env_failed",
        );
        assert.equal(failure?.reason, reason, result.stderr);
      });
    }
  },
);

test("invalid launcher arguments exit with status 2", posixOnly, async (t) => {
  const f = fixture(t);
  const result = await startLauncher(f, ["--path=api", ...commandArgs(0)])
    .exited;
  assert.equal(result.code, 2, result.stderr);
  assert.equal(existsSync(`${f.out}.start`), false);
  assert.equal(records(result.stderr)[0]?.event, "dev_env_usage_error");
});

test(
  "a terminal interrupt reaches the command exactly once",
  posixOnly,
  async (t) => {
    for (const config of [null, ProjectConfigFile.Yaml]) {
      await t.test(config ?? "direct", async (t) => {
        const f = fixture(t, { config });
        const launcher = startLauncher(f, [
          "--path=/api",
          ...commandArgs(0, "wait"),
        ]);
        await waitFor(() => launcher.output().includes("ready"), "the command");
        // A terminal Ctrl-C signals the whole foreground process group.
        process.kill(-launcher.child.pid, "SIGINT");
        const result = await launcher.exited;
        // The launcher waits for the command and exits with its status.
        assert.equal(result.code, 0, result.stderr);
        assert.equal(readFileSync(`${f.out}.signals`, "utf8"), "SIGINT\n");
      });
    }
  },
);

test(
  "a forced stop of the process group also stops the command",
  posixOnly,
  async (t) => {
    // Turborepo force-stops a task with SIGKILL to the task's process group.
    for (const config of [null, ProjectConfigFile.Yaml]) {
      await t.test(config ?? "direct", async (t) => {
        const f = fixture(t, { config });
        const launcher = startLauncher(f, [
          "--path=/api",
          ...commandArgs(0, "wait"),
        ]);
        await waitFor(() => launcher.output().includes("ready"), "the command");
        const { pid } = started(f);
        process.kill(-launcher.child.pid, "SIGKILL");
        try {
          // Check the command first: a command that survives keeps the
          // launcher's output pipes open, so `exited` would never resolve.
          await waitFor(() => !alive(pid), "the command to stop");
        } finally {
          if (alive(pid)) process.kill(pid, "SIGKILL");
        }
        await launcher.exited;
      });
    }
  },
);

test(
  "a signal during the fetch prevents the command from starting",
  posixOnly,
  async (t) => {
    const f = fixture(t, {
      config: ProjectConfigFile.Yaml,
      env: { FAKE_INFISICAL_DELAY_MS: "5000" },
    });
    const launcher = startLauncher(f, ["--path=/api", ...commandArgs(0)]);
    await waitFor(() => existsSync(f.infisicalArgs), "the fetch");
    process.kill(-launcher.child.pid, "SIGINT");
    const result = await launcher.exited;
    assert.equal(result.code, 130, result.stderr);
    assert.equal(existsSync(`${f.out}.start`), false);
  },
);
