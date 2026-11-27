// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "delidev-tauri-test-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const configDir = join(root, "src-tauri");
  const npmCli = join(root, "node_modules", "@tauri-apps", "cli");
  mkdirSync(configDir, { recursive: true });
  mkdirSync(npmCli, { recursive: true });
  copyFileSync(
    new URL("./tauri.mjs", import.meta.url),
    join(root, "tauri.mjs"),
  );

  writeFileSync(
    join(configDir, "tauri.conf.json"),
    JSON.stringify({
      build: { beforeBuildCommand: "pnpm build", frontendDist: "../dist" },
      bundle: { active: true, targets: "all" },
    }),
  );
  writeFileSync(
    join(configDir, "tauri.linux.conf.json"),
    JSON.stringify({
      build: { devUrl: "http://127.0.0.1:13200" },
      bundle: { cef: { embed: false }, targets: ["deb"] },
    }),
  );

  writeFileSync(join(npmCli, "package.json"), '{"main":"index.js"}');
  writeFileSync(
    join(npmCli, "index.js"),
    'exports.run = (args) => console.log(JSON.stringify({ cli: "stock", args }));',
  );
  writeFileSync(
    join(root, "tauri-debian-cli.mjs"),
    'import { fileURLToPath } from "node:url"; export const debianCli = () => fileURLToPath(new URL("./cargo-tauri", import.meta.url));',
  );
  writeFileSync(
    join(root, "cargo-tauri"),
    '#!/usr/bin/env node\nconsole.log(JSON.stringify({ cli: "patched", args: process.argv.slice(2) }));\n',
    { mode: 0o755 },
  );

  return (...args) =>
    JSON.parse(
      execFileSync(process.execPath, ["tauri.mjs", ...args], {
        cwd: root,
        encoding: "utf8",
      }),
    );
}

const linuxOnly = { skip: process.platform !== "linux" };

test(
  "uses the stock CLI for shared CEF and selections without Debian",
  linuxOnly,
  (t) => {
    const run = fixture(t);
    const embed = '{"bundle":{"cef":{"embed":true}}}';
    const rpm = '{"bundle":{"targets":["rpm"]}}';
    for (const args of [
      ["bundle", "--bundles", "deb"],
      ["bundle", "--config", embed, "--config", rpm],
      ["bundle", "--config", embed, "--bundles", "rpm"],
      ["bundle", "--config", embed, "--bundles"],
      ["build", "--config", embed, "--no-bundle"],
      ["build", "--config", '{"bundle":{"active":false,"cef":null}}'],
      ["build", "--target", "aarch64-linux-android", "--bundles", "deb"],
    ]) {
      assert.deepEqual(run(...args), { cli: "stock", args });
    }
  },
);

test(
  "applies platform and CLI bundle overrides before selecting the backport",
  linuxOnly,
  (t) => {
    const run = fixture(t);
    const embed = '{"bundle":{"cef":{"embed":true}}}';
    const rpm = '{"bundle":{"targets":["rpm"]}}';
    for (const args of [
      ["bundle", "--config", embed],
      [
        "bundle",
        "--config",
        embed,
        "--config",
        rpm,
        "--config",
        '{"bundle":{"targets":["deb","rpm"]}}',
      ],
      ["bundle", "--config", embed, "--config", rpm, "-bdeb"],
      [
        "bundle",
        "--config",
        '{"bundle":{"cef":{"embed":null},"targets":null}}',
      ],
    ]) {
      const result = run(...args);
      assert.equal(result.cli, "patched");
      const hookIndex = result.args.findIndex((arg) =>
        arg.includes("cef-deb-notices.mjs"),
      );
      assert.ok(hookIndex > 3);
      assert.deepEqual(
        [
          ...result.args.slice(3, hookIndex - 1),
          ...result.args.slice(hookIndex + 1),
        ],
        args.slice(1),
      );
      const files = JSON.parse(result.args[2]).bundle.linux.deb.files;
      assert.deepEqual(Object.keys(files), [
        "/usr/share/doc/delidev/cef/LICENSE.txt",
        "/usr/share/doc/delidev/cef/CREDITS.html",
      ]);
    }
  },
);

test(
  "preserves a cleared build section when adding the notice hook",
  linuxOnly,
  (t) => {
    const run = fixture(t);
    const embed = '{"bundle":{"cef":{"embed":true}}}';
    const clearedBuild = '{"build":null}';
    const result = run("build", "--config", embed, "--config", clearedBuild);
    const build = JSON.parse(result.args.at(-1)).build;
    assert.deepEqual(Object.keys(build).sort(), [
      "beforeBuildCommand",
      "beforeBundleCommand",
      "devUrl",
      "frontendDist",
    ]);
    assert.equal(build.beforeBuildCommand, null);
    assert.equal(build.devUrl, null);
    assert.equal(build.frontendDist, null);
    assert.match(build.beforeBundleCommand, /cef-deb-notices\.mjs/);

    const custom =
      '{"build":{"frontendDist":"../custom-dist","beforeBundleCommand":{"script":"echo custom","cwd":"."}}}';
    const restored = run(
      "build",
      "--config",
      embed,
      "--config",
      clearedBuild,
      "--config",
      custom,
    );
    assert.equal(restored.args.at(-3), custom);
    const restoredBuild = JSON.parse(restored.args.at(-1)).build;
    assert.deepEqual(Object.keys(restoredBuild), ["beforeBundleCommand"]);
    assert.equal(restoredBuild.beforeBundleCommand.cwd, ".");
    assert.match(restoredBuild.beforeBundleCommand.script, /before-bundle\.sh/);
  },
);
