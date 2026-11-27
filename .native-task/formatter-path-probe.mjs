import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { delimiter, dirname, isAbsolute, join } from "node:path";

const [mode, gitPath] = process.argv.slice(2);
assert.ok(
  ["baseline", "focused", "full"].includes(mode),
  "Select baseline, focused or full",
);
assert.equal(Number(process.versions.node.split(".")[0]), 24, "Use Node 24");
assert.ok(
  gitPath && isAbsolute(gitPath) && existsSync(gitPath),
  "Supply the absolute Git executable path",
);

const windows = process.platform === "win32";
const gitDirectory = dirname(gitPath);
for (const prefix of [
  process.cwd(),
  dirname(process.execPath),
  join(process.cwd(), "node_modules", ".bin"),
]) {
  for (const name of windows ? ["git.exe", "git.com"] : ["git"]) {
    assert.equal(
      existsSync(join(prefix, name)),
      false,
      `Git must be outside ${prefix}`,
    );
  }
}

const pathKey = windows ? "Path" : "PATH";
const env = { ...process.env };
for (const key of Object.keys(env)) {
  if (
    ["PATH", "NODE_OPTIONS", "NODE_COMPILE_CACHE", "NODE_V8_COVERAGE"].includes(
      key.toUpperCase(),
    )
  ) {
    delete env[key];
  }
}
if (windows) {
  assert.ok(process.env.SystemRoot, "SystemRoot is required");
  env[pathKey] = [
    gitDirectory,
    join(process.env.SystemRoot, "System32"),
    process.env.SystemRoot,
  ].join(delimiter);
} else {
  env[pathKey] = [gitDirectory, "/usr/bin", "/bin"].join(delimiter);
}

const preflight = `
  import assert from "node:assert/strict";
  import { execFileSync } from "node:child_process";
  const keys = Object.keys(process.env).filter((key) => key.toUpperCase() === "PATH");
  assert.deepEqual(keys, [${JSON.stringify(pathKey)}]);
  const git = execFileSync("git", ["--version"], { encoding: "utf8" }).trim();
  const [, major, minor] = /^git version (\\d+)\\.(\\d+)/.exec(git) ?? [];
  assert.ok(Number(major) > 2 || (Number(major) === 2 && Number(minor) >= 32), git);
  console.log("PROBE preflight " + process.execPath + " " + keys[0] + " " + git);
`;
const hooked =
  "^pre-commit formats and stages initial-commit files with literal paths$";
const focused = [
  hooked,
  "^fixture preserves inherited search directories for Windows PATH spellings$",
  "^fixture uses case-sensitive PATH handling on Unix$",
  "^fixture ignores hostile (global Git configuration and GIT_\\*|LEFTHOOK\\*) variables$",
].join("|");
const flags =
  mode === "full"
    ? []
    : [`--test-name-pattern=${mode === "baseline" ? hooked : focused}`];
const result = spawnSync(
  process.execPath,
  [
    "--import",
    `data:text/javascript,${encodeURIComponent(preflight)}`,
    "--test",
    "--test-reporter=spec",
    ...flags,
    "scripts/ci/format-changed.test.mjs",
  ],
  { env, encoding: "utf8", maxBuffer: 10 * 1024 * 1024 },
);
process.stdout.write(result.stdout ?? "");
process.stderr.write(result.stderr ?? "");
if (result.error) throw result.error;
assert.match(
  result.stdout,
  new RegExp(`PROBE preflight .* ${pathKey} git version`),
);
if (mode === "baseline") {
  assert.equal(result.status, 1);
  assert.match(result.stdout + result.stderr, /spawnSync git ENOENT/);
  assert.match(
    result.stdout + result.stderr,
    /spawnargs:\s*\[ ['"]init['"], ['"]--quiet['"] \]/,
  );
  console.log(
    "PROBE baseline reproduced: Git version check passed, then fixture git init failed with ENOENT",
  );
} else {
  assert.equal(result.status, 0);
  assert.match(
    result.stdout,
    /✔ fixture preserves inherited search directories for Windows PATH spellings/,
  );
  assert.match(
    result.stdout,
    /✔ pre-commit formats and stages initial-commit files with literal paths/,
  );
  console.log(
    `PROBE ${mode} passed: Git initialized the fixture, Rstack formatted the staged files and Lefthook committed them`,
  );
}
