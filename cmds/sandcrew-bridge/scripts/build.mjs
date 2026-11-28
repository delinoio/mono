// SPDX-License-Identifier: Apache-2.0
//
// Cross-compiles the SandCrew guest bridge for every release target into
// dist/<target>/. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#build-and-test. sandcrew-api embeds
// these binaries, so its build sets the version (its own package version) and
// the commit; a local build uses 0.0.0-dev and the checkout's HEAD. The
// package script is `build:binaries`, not `build`, so that the `turbo run
// build` of the CI workspace jobs does not cross-compile release binaries that
// no pull request check uses.
//
// Usage: node scripts/build.mjs [--out <dir>] [--version <v>] [--commit <sha>]
// SANDCREW_BRIDGE_VERSION and SANDCREW_BRIDGE_COMMIT are the defaults of
// --version and --commit.
import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const versionPackage =
  "github.com/delinoio/mono/cmds/sandcrew-bridge/internal/version";

// The release targets; keep them equal to Targets in internal/pins/pins.go.
export const targets = [
  { goos: "linux", goarch: "amd64" },
  { goos: "linux", goarch: "arm64" },
  { goos: "darwin", goarch: "arm64" },
  { goos: "windows", goarch: "amd64" },
  { goos: "windows", goarch: "arm64" },
];

// Returns the go build invocations for the options.
export function buildPlan({ out, version, commit }) {
  const ldflags = `-s -w -X ${versionPackage}.Version=${version} -X ${versionPackage}.Commit=${commit}`;
  return targets.map(({ goos, goarch }) => {
    const binary =
      goos === "windows" ? "sandcrew-bridge.exe" : "sandcrew-bridge";
    const output = join(out, `${goos}-${goarch}`, binary);
    return {
      target: `${goos}-${goarch}`,
      output,
      args: [
        "build",
        "-buildvcs=false",
        "-trimpath",
        "-ldflags",
        ldflags,
        "-o",
        output,
        ".",
      ],
      env: { CGO_ENABLED: "0", GOOS: goos, GOARCH: goarch },
    };
  });
}

// Parses the command line. Unknown arguments fail.
export function parseArgs(argv, env) {
  const options = {
    out: join(packageDir, "dist"),
    version: env.SANDCREW_BRIDGE_VERSION || "0.0.0-dev",
    commit: env.SANDCREW_BRIDGE_COMMIT || "",
  };
  for (let i = 0; i < argv.length; i += 2) {
    const name = argv[i];
    const value = argv[i + 1];
    if (
      !["--out", "--version", "--commit"].includes(name) ||
      value === undefined
    ) {
      throw new Error(`build: unknown or incomplete argument ${name}`);
    }
    options[name.slice(2)] = name === "--out" ? resolve(value) : value;
  }
  for (const key of ["version", "commit"]) {
    // The values enter -ldflags; keep them to version and SHA characters.
    if (options[key] !== "" && !/^[0-9A-Za-z.+-]+$/.test(options[key])) {
      throw new Error(`build: invalid ${key} ${options[key]}`);
    }
  }
  return options;
}

function headCommit() {
  try {
    return execFileSync("git", ["rev-parse", "HEAD"], {
      cwd: packageDir,
      encoding: "utf8",
    }).trim();
  } catch {
    return "unknown";
  }
}

function main() {
  const options = parseArgs(process.argv.slice(2), process.env);
  if (options.commit === "") options.commit = headCommit();
  rmSync(options.out, { recursive: true, force: true });
  for (const step of buildPlan(options)) {
    mkdirSync(dirname(step.output), { recursive: true });
    console.log(
      JSON.stringify({
        event: "sandcrew_bridge_build",
        target: step.target,
        version: options.version,
        commit: options.commit,
      }),
    );
    execFileSync("go", step.args, {
      cwd: packageDir,
      env: { ...process.env, ...step.env },
      stdio: "inherit",
    });
  }
}

if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  main();
}
