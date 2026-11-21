// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
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
  InjectionMode,
  launchPlan,
  parseArguments,
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

// A fake `infisical` CLI. Like the real CLI (`infisical run`), it adds the
// fetched variables over the inherited environment, starts the command after
// `--` in its own process group and forwards every stop signal to it. It
// records its arguments so that tests can check the exact invocation. The
// file has no extension, so Node.js runs it as CommonJS.
const fakeInfisical = `#!/usr/bin/env node
const { spawn } = require("node:child_process");
const { writeFileSync } = require("node:fs");
const args = process.argv.slice(2);
writeFileSync(process.env.FAKE_INFISICAL_ARGS, JSON.stringify(args));
const command = args.slice(args.indexOf("--") + 1);
const child = spawn(command[0], command.slice(1), {
  stdio: "inherit",
  shell: false,
  env: { ...process.env, INJECTED_BY_FAKE_INFISICAL: "yes" },
});
for (const signal of ["SIGINT", "SIGTERM", "SIGQUIT", "SIGHUP"]) {
  process.on(signal, () => child.kill(signal));
}
child.on("close", (code, signal) => process.exit(code ?? 255));
`;

// The development command. It records its arguments and the injected marker,
// prints a ready line, and on SIGINT records the signal and exits shortly
// afterwards, so that a duplicate delivery is also recorded.
const devCommand = `import { appendFileSync, writeFileSync } from "node:fs";
const out = process.env.DEV_COMMAND_OUT;
writeFileSync(out + ".start", JSON.stringify({
  args: process.argv.slice(2),
  injected: process.env.INJECTED_BY_FAKE_INFISICAL ?? null,
}));
process.on("SIGINT", () => {
  appendFileSync(out + ".signals", "SIGINT\\n");
  setTimeout(() => process.exit(0), 300);
});
const exitCode = Number(process.argv[2] ?? "0");
if (process.argv[3] === "wait") {
  console.log("ready");
  setInterval(() => {}, 1000);
} else {
  process.exit(exitCode);
}
`;

// Builds a repository-like directory with the launcher at
// `scripts/dev/with-infisical.mjs`, so that the launcher resolves the
// fixture as its repository root.
function fixture(t, { config = null, cli = true } = {}) {
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
  writeFileSync(join(root, "dev-command.mjs"), devCommand);
  const out = join(root, "out");
  return {
    root,
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
    },
  };
}

// Starts the launcher in its own process group, as a terminal starts a
// foreground job, and collects its output.
function startLauncher(f, args) {
  const child = spawn(process.execPath, [f.launcher, ...args], {
    cwd: f.root,
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

async function waitFor(predicate, what) {
  const deadline = Date.now() + 10_000;
  while (!predicate()) {
    if (Date.now() > deadline) assert.fail(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

test("parseArguments accepts one folder and a command", () => {
  assert.deepEqual(
    parseArguments(["--path=/accounts-api", "--", "node", "dev.mjs", "--x"]),
    { folder: "/accounts-api", command: ["node", "dev.mjs", "--x"] },
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
    {
      folder: "/a",
      command: ["x", "--", "--path=/b"],
    },
  );
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

test("launchPlan pins the dev environment and the package folder", () => {
  const command = ["node", "dev.mjs", "--flag"];
  assert.deepEqual(
    launchPlan({ root: "/repo", folder: "/api", command, configFile: null }),
    {
      mode: InjectionMode.Direct,
      executable: "node",
      args: ["dev.mjs", "--flag"],
    },
  );
  assert.equal(DevelopmentEnvironment, "dev");
  assert.deepEqual(
    launchPlan({
      root: "/repo",
      folder: "/api",
      command,
      configFile: ProjectConfigFile.Yaml,
    }),
    {
      mode: InjectionMode.Infisical,
      executable: "infisical",
      args: [
        "run",
        "--env=dev",
        "--project-config-dir=/repo",
        "--path=/api",
        "--",
        "node",
        "dev.mjs",
        "--flag",
      ],
    },
  );
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
    const start = JSON.parse(readFileSync(`${f.out}.start`, "utf8"));
    assert.deepEqual(start, { args: ["7"], injected: null });
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
  "with a project configuration the command runs through infisical run",
  posixOnly,
  async (t) => {
    for (const config of Object.values(ProjectConfigFile)) {
      await t.test(config, async (t) => {
        const f = fixture(t, { config });
        const result = await startLauncher(f, [
          "--path=/accounts-api",
          ...commandArgs(3),
        ]).exited;
        assert.equal(result.code, 3, result.stderr);
        assert.deepEqual(JSON.parse(readFileSync(f.infisicalArgs, "utf8")), [
          "run",
          "--env=dev",
          `--project-config-dir=${f.root}`,
          "--path=/accounts-api",
          "--",
          process.execPath,
          "dev-command.mjs",
          "3",
        ]);
        const start = JSON.parse(readFileSync(`${f.out}.start`, "utf8"));
        assert.deepEqual(start, { args: ["3"], injected: "yes" });
        assert.deepEqual(records(result.stderr), [
          {
            event: "dev_env_infisical",
            environment: "dev",
            folder: "/accounts-api",
            config,
          },
        ]);
      });
    }
  },
);

test(
  "a configured checkout without the CLI fails before the command runs",
  posixOnly,
  async (t) => {
    const f = fixture(t, { config: ProjectConfigFile.Yaml, cli: false });
    const result = await startLauncher(f, ["--path=/api", ...commandArgs(0)])
      .exited;
    assert.equal(result.code, 1, result.stderr);
    assert.equal(existsSync(`${f.out}.start`), false);
    const failure = records(result.stderr).find(
      (record) => record.event === "dev_env_failed",
    );
    assert.equal(failure?.reason, "infisical_cli_missing", result.stderr);
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
        assert.equal(result.code, 130, result.stderr);
        assert.equal(readFileSync(`${f.out}.signals`, "utf8"), "SIGINT\n");
      });
    }
  },
);
