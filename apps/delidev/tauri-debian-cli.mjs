// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join } from "node:path";

const cli = {
  name: "tauri-cli",
  version: "3.0.0-alpha.6",
  sha256: "f10097923ce015a8a11d5fd840acbaa5b6081a441d065b04f840f995a122cdba",
};
const bundler = {
  name: "tauri-bundler",
  version: "3.0.0-alpha.5",
  sha256: "3584509319122de69155798e3bf6ab931e1bb9165d80e7536cb53ab6ce7bb6f6",
};

// Debian portion of Tauri commit 6620118f0fe85472abfdccea8d99fd0ec9447c3f.
// CEF 154 no longer ships standalone ANGLE libraries. Remove this backport
// when the published CLI pin includes the fix.
const angleFiles =
  '      // ANGLE support\n      "libEGL.so",\n      "libGLESv2.so",\n';

async function unpackCrate(workDir, { name, version, sha256 }) {
  const packageName = `${name}-${version}`;
  const url = `https://static.crates.io/crates/${name}/${packageName}.crate`;
  const response = await fetch(url);
  if (!response.ok) {
    throw new Error(
      `Could not download ${packageName}: HTTP ${response.status}`,
    );
  }
  const bytes = Buffer.from(await response.arrayBuffer());
  if (createHash("sha256").update(bytes).digest("hex") !== sha256) {
    throw new Error(`Checksum mismatch for ${packageName}`);
  }

  const archive = join(workDir, `${packageName}.crate`);
  writeFileSync(archive, bytes);
  execFileSync("tar", ["-xzf", archive, "-C", workDir], { stdio: "inherit" });
  return join(workDir, packageName);
}

export async function debianCli() {
  const manifest = JSON.parse(
    readFileSync(new URL("./package.json", import.meta.url)),
  );
  if (manifest.devDependencies["@tauri-apps/cli"] !== cli.version) {
    throw new Error("Review the Debian backport for the new Tauri CLI version");
  }
  const rustc = execFileSync("rustc", ["-vV"], { encoding: "utf8" });
  const host = /^host: (.+)$/m.exec(rustc)?.[1];
  if (!host) throw new Error("Could not determine the Rust host target");

  const { XDG_CACHE_HOME } = process.env;
  const cache =
    XDG_CACHE_HOME && isAbsolute(XDG_CACHE_HOME)
      ? XDG_CACHE_HOME
      : join(homedir(), ".cache");
  const cacheVersion = createHash("sha256")
    .update(cli.sha256)
    .update(bundler.sha256)
    .update(angleFiles)
    .digest("hex");
  const cacheDir = join(
    cache,
    "delidev",
    "tauri-debian-cli",
    host,
    cacheVersion,
  );
  const executable = join(cacheDir, "cargo-tauri");
  if (existsSync(executable)) return executable;

  mkdirSync(cacheDir, { recursive: true });
  const workDir = mkdtempSync(join(cacheDir, "build-"));
  try {
    console.error(
      `Building Tauri CLI ${cli.version} with the Debian CEF backport`,
    );
    const cliDir = await unpackCrate(workDir, cli);
    const bundlerDir = await unpackCrate(workDir, bundler);
    const debianSource = join(bundlerDir, "src/bundle/linux/debian.rs");
    const source = readFileSync(debianSource, "utf8");
    if (!source.includes(angleFiles)) {
      throw new Error("Could not locate the pinned Debian ANGLE copy list");
    }
    writeFileSync(debianSource, source.replace(angleFiles, ""));

    // Keep the published lockfile and dependency graph. An empty workspace
    // isolates the extracted CLI when the cache is inside a Cargo workspace.
    const cliManifest = join(cliDir, "Cargo.toml");
    const originalManifest = readFileSync(cliManifest, "utf8");
    writeFileSync(cliManifest, `${originalManifest}\n[workspace]\n`);
    const targetDir = join(workDir, "target");
    execFileSync(
      "cargo",
      [
        "build",
        "--locked",
        "--release",
        "--bin",
        "cargo-tauri",
        "--manifest-path",
        cliManifest,
        "--target",
        host,
        "--target-dir",
        targetDir,
        "--config",
        `paths = [${JSON.stringify(bundlerDir)}]`,
      ],
      { stdio: "inherit" },
    );

    const stagedCli = join(workDir, "cargo-tauri");
    copyFileSync(join(targetDir, host, "release", "cargo-tauri"), stagedCli);
    renameSync(stagedCli, executable);
    return executable;
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
}
