// SPDX-License-Identifier: Apache-2.0

// A developer's global or system Git configuration (core.hooksPath,
// commit.gpgsign, ...), their global ignore and attributes files, and the GIT_*
// variables of an enclosing Git hook invocation (GIT_DIR, GIT_INDEX_FILE, ...)
// must not change how fixture repositories behave. Shared by the scripts/ci
// test suites; this module is not a test file, so
// `node --test scripts/ci/*.test.mjs` does not collect it.
// Mutates and returns `env`.
const nullDevice = process.platform === "win32" ? "NUL" : "/dev/null";

export function isolateGitEnvironment(env) {
  for (const name of Object.keys(env)) {
    if (name.startsWith("GIT_") || name === "LEFTHOOK_CONFIG") {
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
  // fixture environment built here. Requires Git 2.31 or newer for
  // GIT_CONFIG_COUNT. Remove once Git offers a single switch that disables
  // the XDG default ignore and attributes files.
  env.GIT_CONFIG_COUNT = "2";
  env.GIT_CONFIG_KEY_0 = "core.excludesFile";
  env.GIT_CONFIG_VALUE_0 = nullDevice;
  env.GIT_CONFIG_KEY_1 = "core.attributesFile";
  env.GIT_CONFIG_VALUE_1 = nullDevice;
  return env;
}
