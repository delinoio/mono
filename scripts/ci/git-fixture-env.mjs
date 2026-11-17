// SPDX-License-Identifier: Apache-2.0

// Requires Git 2.32 or newer: GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM, which
// disable the per-user configuration, are ignored by older Git, and
// GIT_CONFIG_COUNT needs 2.31. `isolateGitEnvironment` therefore fails loudly
// on older Git instead of silently reading the developer's ~/.gitconfig.
//
// A developer's global or system Git configuration (core.hooksPath,
// commit.gpgsign, ...), their global ignore and attributes files, the system
// attributes file, and the GIT_*
// variables of an enclosing Git hook invocation (GIT_DIR, GIT_INDEX_FILE, ...)
// must not change how fixture repositories behave. The same holds for every
// LEFTHOOK* variable (LEFTHOOK=0, LEFTHOOK_EXCLUDE, LEFTHOOK_OUTPUT,
// LEFTHOOK_CONFIG, ...), which would disable, skip or reconfigure the
// generated hooks. Callers that need a Lefthook variable, such as
// LEFTHOOK_BIN, set it on the returned environment. Shared by the scripts/ci
// test suites; this module is not a test file, so
// `node --test scripts/ci/*.test.mjs` does not collect it.
// Mutates and returns `env`.
import { execFileSync } from "node:child_process";

const nullDevice = process.platform === "win32" ? "NUL" : "/dev/null";

const minimumGitMajor = 2;
const minimumGitMinor = 32;

// Parses `git --version` output such as `git version 2.43.0`,
// `git version 2.43.0.windows.1`, `git version 2.43.0.vfs.0.1` or
// `git version 2.39.5 (Apple Git-154)`. Returns `{ major, minor }` or null.
export function parseGitVersion(output) {
  const match = /^git version (\d+)\.(\d+)(?:[.\s]|$)/.exec(
    String(output).trim(),
  );
  return match ? { major: Number(match[1]), minor: Number(match[2]) } : null;
}

let gitVersionChecked = false;

// Throws when the installed Git is older than 2.32 or its version output
// cannot be parsed. Runs `git --version` once per process.
export function assertSupportedGitVersion() {
  if (gitVersionChecked) {
    return;
  }
  const output = execFileSync("git", ["--version"], {
    encoding: "utf8",
    shell: false,
  }).trim();
  const version = parseGitVersion(output);
  if (!version) {
    throw new Error(
      `Cannot parse the Git version from ${JSON.stringify(output)}; the scripts/ci fixtures require Git ${minimumGitMajor}.${minimumGitMinor} or newer because older Git ignores GIT_CONFIG_GLOBAL.`,
    );
  }
  if (
    version.major < minimumGitMajor ||
    (version.major === minimumGitMajor && version.minor < minimumGitMinor)
  ) {
    throw new Error(
      `Found Git ${version.major}.${version.minor}; the scripts/ci fixtures require Git ${minimumGitMajor}.${minimumGitMinor} or newer because older Git ignores GIT_CONFIG_GLOBAL and would read your global Git configuration. Upgrade Git and retry.`,
    );
  }
  gitVersionChecked = true;
}

export function isolateGitEnvironment(env) {
  assertSupportedGitVersion();
  for (const name of Object.keys(env)) {
    if (name.startsWith("GIT_") || name.startsWith("LEFTHOOK")) {
      delete env[name];
    }
  }
  env.GIT_CONFIG_GLOBAL = nullDevice;
  env.GIT_CONFIG_NOSYSTEM = "1";
  // Workaround: GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM only disable the
  // global and system configuration files. Git still reads its built-in
  // default ignore and attributes files under $XDG_CONFIG_HOME/git (or
  // $HOME/.config/git) when core.excludesFile and core.attributesFile are
  // unset, so a developer's global rules could hide fixture files or change
  // their text conversion. Pointing both settings at the null device through
  // command-scoped configuration replaces those defaults. Scope: every
  // fixture environment built here. Requires Git 2.32 or newer overall
  // (GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM need 2.32; GIT_CONFIG_COUNT
  // needs 2.31). Remove once Git offers a single switch that disables
  // the XDG default ignore and attributes files.
  //
  // Workaround: the system attributes file ($(prefix)/etc/gitattributes) is a
  // separate attributes frame that neither GIT_CONFIG_NOSYSTEM nor
  // core.attributesFile covers, so a host-level rule could still change fixture
  // file bytes. GIT_ATTR_NOSYSTEM=1 is the only switch for it. The variable is
  // undocumented but stable since Git 1.7.4, so the Git 2.32 minimum
  // (GIT_CONFIG_GLOBAL needs 2.32; GIT_CONFIG_COUNT needs 2.31) stays the
  // binding minimum. Scope: every fixture
  // environment built here; callers cannot override it because the loop above
  // removes it. Remove once Git offers a single switch that disables every
  // default ignore and attributes file.
  env.GIT_ATTR_NOSYSTEM = "1";
  env.GIT_CONFIG_COUNT = "2";
  env.GIT_CONFIG_KEY_0 = "core.excludesFile";
  env.GIT_CONFIG_VALUE_0 = nullDevice;
  env.GIT_CONFIG_KEY_1 = "core.attributesFile";
  env.GIT_CONFIG_VALUE_1 = nullDevice;
  return env;
}
