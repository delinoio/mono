// SPDX-License-Identifier: Apache-2.0

// A developer's global or system Git configuration (core.hooksPath,
// commit.gpgsign, ...) and the GIT_* variables of an enclosing Git hook
// invocation (GIT_DIR, GIT_INDEX_FILE, ...) must not change how fixture
// repositories behave. Shared by the scripts/ci test suites; this module is not
// a test file, so `node --test scripts/ci/*.test.mjs` does not collect it.
// Mutates and returns `env`.
export function isolateGitEnvironment(env) {
  for (const name of Object.keys(env)) {
    if (name.startsWith("GIT_") || name === "LEFTHOOK_CONFIG") {
      delete env[name];
    }
  }
  env.GIT_CONFIG_GLOBAL = process.platform === "win32" ? "NUL" : "/dev/null";
  env.GIT_CONFIG_NOSYSTEM = "1";
  return env;
}
