// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { createReadStream, existsSync } from "node:fs";
import { copyFile, readFile, realpath, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const {
  CEF_PATH,
  DELIDEV_CEF_NOTICE_DIR,
  TAURI_ENV_TARGET_TRIPLE,
  XDG_CACHE_HOME,
} = process.env;
const tauriDir = fileURLToPath(new URL("./src-tauri/", import.meta.url));
const cargoLock = new URL("../../Cargo.lock", import.meta.url);
const licenseName = "LICENSE.txt";
const creditsName = "CREDITS.html";

async function resolveDistribution() {
  const lock = await readFile(cargoLock, "utf8");
  const [, version] =
    /^\[\[package\]\]\nname = "cef"\nversion = "([^"]+)"/m.exec(lock) ?? [];
  if (!version) throw new Error("Cargo.lock does not contain the CEF version");

  // Match the pinned Tauri CLI resolver. It interprets a relative CEF_PATH
  // from src-tauri and prefers the versioned directory over a direct path.
  const cefVersion = version.split("+")[1] ?? version;
  const arch = TAURI_ENV_TARGET_TRIPLE.split("-")[0];
  const cache =
    XDG_CACHE_HOME && isAbsolute(XDG_CACHE_HOME)
      ? XDG_CACHE_HOME
      : join(homedir(), ".cache");
  const cefPath = resolve(tauriDir, CEF_PATH ?? join(cache, "tauri-cef"));
  const versioned = join(cefPath, cefVersion, `cef_linux_${arch}`);
  const distribution = existsSync(versioned) ? versioned : cefPath;
  if (!existsSync(join(distribution, "libcef.so"))) {
    throw new Error(`no libcef.so in ${distribution}`);
  }
  return realpath(distribution);
}

async function readArchivedLicense(distribution) {
  // download-cef 3.0.1 retains CREDITS.html and the verified archive, but
  // discards LICENSE.txt when it flattens the archive into the cache.
  const { name, sha1 } = JSON.parse(
    await readFile(join(distribution, "archive.json"), "utf8"),
  );
  if (
    !/^cef_binary_[^/]+\.tar\.bz2$/.test(name) ||
    !/^[a-f0-9]{40}$/.test(sha1)
  ) {
    throw new Error(`invalid CEF archive metadata in ${distribution}`);
  }
  const archive = join(dirname(distribution), name);
  const hash = createHash("sha1");
  for await (const chunk of createReadStream(archive)) hash.update(chunk);
  if (hash.digest("hex") !== sha1)
    throw new Error(`CEF archive checksum mismatch for ${archive}`);

  const member = join(name.slice(0, -".tar.bz2".length), licenseName);
  return execFileSync("tar", [
    "--extract",
    "--to-stdout",
    "--occurrence=1",
    "--file",
    archive,
    "--",
    member,
  ]);
}

if (TAURI_ENV_TARGET_TRIPLE?.includes("-linux-")) {
  try {
    const distribution = await resolveDistribution();
    const license = join(distribution, licenseName);
    const output = join(DELIDEV_CEF_NOTICE_DIR, licenseName);
    await copyFile(
      join(distribution, creditsName),
      join(DELIDEV_CEF_NOTICE_DIR, creditsName),
    );
    if (existsSync(license)) {
      await copyFile(license, output);
    } else {
      await writeFile(output, await readArchivedLicense(distribution));
    }
  } catch (error) {
    console.error(
      `Cannot bundle the original CEF notices for Debian: ${error.message}`,
    );
    process.exitCode = 1;
  }
}
