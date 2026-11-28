// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import {
  copyFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { patchCefLinuxDependencies } from "./tauri-debian-cli.mjs";

function fixture(t, platform = process.platform) {
  const workspace = mkdtempSync(join(tmpdir(), "delidev-tauri-test-"));
  const root = join(workspace, "apps", "delidev");
  t.after(() => rmSync(workspace, { recursive: true, force: true }));
  const configDir = join(root, "src-tauri");
  const npmCli = join(root, "node_modules", "@tauri-apps", "cli");
  const staging = join(root, "temp");
  mkdirSync(configDir, { recursive: true });
  mkdirSync(npmCli, { recursive: true });
  mkdirSync(staging);
  for (const file of ["tauri.mjs", "cef-notices.mjs"])
    copyFileSync(new URL(`./${file}`, import.meta.url), join(root, file));
  writeFileSync(
    join(root, "platform.cjs"),
    `Object.defineProperty(process, "platform", { value: ${JSON.stringify(platform)} });`,
  );
  writeFileSync(
    join(workspace, "Cargo.lock"),
    '[[package]]\nname = "cef"\nversion = "1.2.3+4.5.6"\n',
  );

  writeFileSync(
    join(configDir, "tauri.conf.json"),
    JSON.stringify({
      build: { beforeBuildCommand: "pnpm build", frontendDist: "../dist" },
      bundle: { active: true, targets: "all" },
    }),
  );
  writeFileSync(
    join(configDir, "tauri.macos.conf.json"),
    JSON.stringify({ bundle: { cef: { embed: false }, targets: ["dmg"] } }),
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
    join(npmCli, "tauri.js"),
    `const { spawnSync } = require("node:child_process");
const { copyFileSync, mkdirSync } = require("node:fs");
const { dirname, join } = require("node:path");
const args = process.argv.slice(2);
const patch = JSON.parse(args[args.lastIndexOf("--config") + 1]);
const hook = patch.build.beforeBundleCommand;
if (process.env.DELIDEV_TEST_RUN_HOOK) {
  const result = spawnSync("sh", ["-c", hook.script ?? hook], {
    cwd: hook.cwd ?? process.cwd(),
    stdio: "inherit",
    env: { ...process.env, TAURI_ENV_TARGET_TRIPLE: "aarch64-apple-darwin" },
  });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
  const files = (patch.bundle.macOS ?? patch.bundle.macos).files;
  for (const [path, source] of Object.entries(files)) {
    if (!source) continue;
    const destination = join("fixture.app", "Contents", path);
    mkdirSync(dirname(destination), { recursive: true });
    copyFileSync(source, destination);
  }
}
console.log(JSON.stringify({ cli: "stock", args }));
`,
  );
  writeFileSync(
    join(root, "tauri-debian-cli.mjs"),
    'import { fileURLToPath } from "node:url"; export const linuxCefCli = () => fileURLToPath(new URL("./cargo-tauri", import.meta.url));',
  );
  writeFileSync(
    join(root, "cargo-tauri"),
    '#!/usr/bin/env node\nconsole.log(JSON.stringify({ cli: "patched", args: process.argv.slice(2) }));\n',
    { mode: 0o755 },
  );

  const launch = (spawner, args, env, options) =>
    spawner(
      process.execPath,
      ["--require", "./platform.cjs", "tauri.mjs", ...args],
      {
        cwd: root,
        encoding: "utf8",
        env: { ...process.env, TMPDIR: staging, ...env },
        ...options,
      },
    );
  const invoke = (args, env = {}) => launch(spawnSync, args, env);
  const start = (args) =>
    launch(
      spawn,
      args,
      {},
      {
        detached: true,
        stdio: ["pipe", "pipe", "inherit"],
      },
    );
  const run = (...args) => {
    const result = invoke(args);
    assert.equal(result.status, 0, result.stderr);
    return JSON.parse(result.stdout);
  };
  return { root, staging, invoke, run, start };
}

const linuxOnly = { skip: process.platform !== "linux" };
const unixOnly = { skip: !["darwin", "linux"].includes(process.platform) };
const embedCef = '{"bundle":{"cef":{"embed":true}}}';

function mobileFixture(t) {
  const { root, invoke, run } = fixture(t);
  const tauri = join(root, "src-tauri");
  const templates = join(root, "templates");
  const androidMain = join("android", "app", "src", "main");
  const iconSet = join("apple", "Assets.xcassets", "AppIcon.appiconset");
  const androidRes = join(tauri, "gen", androidMain, "res");
  const iosIcons = join(tauri, "gen", iconSet);
  const iosNames = ["AppIcon-512@2x.png", "AppIcon-20x20@2x-1.png"];
  const catalog = JSON.stringify({
    images: [...iosNames.map((filename) => ({ filename })), { idiom: "ipad" }],
  });
  const androidLauncher = join("mipmap-mdpi", "ic_launcher.png");
  const stock = "stock";

  for (const [file, content] of [
    [
      join(androidMain, "AndroidManifest.xml"),
      '<application android:icon="@mipmap/ic_launcher" />',
    ],
    [join(androidMain, "res", androidLauncher), stock],
    [join(androidMain, "res", "values", "strings.xml"), "unrelated resource"],
    [join(iconSet, "Contents.json"), catalog],
    ...iosNames.map((name) => [join(iconSet, name), stock]),
  ]) {
    const path = join(templates, file);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
  for (const platform of ["android", "ios"]) {
    cpSync(
      new URL(`./src-tauri/icons/${platform}/`, import.meta.url),
      join(tauri, "icons", platform),
      { recursive: true },
    );
  }

  writeFileSync(
    join(root, "node_modules", "@tauri-apps", "cli", "index.js"),
    `const { cpSync, existsSync } = require("node:fs");
const { join } = require("node:path");
exports.run = async (args) => {
  const [platform, action] = args.filter((arg) => !arg.startsWith("-"));
  const folder = platform === "ios" ? "apple" : platform;
  const host = join("src-tauri", "gen", folder);
  if (action === "init" && !args.includes("--help") && !args.includes("-vh") &&
      !process.env.DELIDEV_TEST_NO_HOST && !existsSync(host)) {
    await new Promise((resolve) => setImmediate(resolve));
    cpSync(join("templates", folder), host, { recursive: true });
    if (process.env.DELIDEV_TEST_FAIL_INIT) throw new Error("Tauri init failed");
  }
  console.log(JSON.stringify({ cli: "stock", args }));
};`,
  );

  return {
    tauri,
    invoke,
    run,
    androidRes,
    androidLauncher,
    iosIcons,
    iosNames,
    catalog,
    stock,
  };
}

test("fresh mobile init installs only the selected host's launcher assets", (t) => {
  const {
    tauri,
    run,
    androidRes,
    androidLauncher,
    iosIcons,
    iosNames,
    catalog,
  } = mobileFixture(t);
  const android = ["android", "-v", "init", "--ci"];
  assert.deepEqual(run(...android), { cli: "stock", args: android });
  assert.equal(existsSync(iosIcons), false);

  const androidSource = join(tauri, "icons", "android");
  const androidFiles = readdirSync(androidSource, {
    recursive: true,
    withFileTypes: true,
  }).filter((entry) => entry.isFile());
  assert.equal(androidFiles.length, 17);
  for (const { name, parentPath } of androidFiles) {
    const resource = join(basename(parentPath), name);
    assert.deepEqual(
      readFileSync(join(androidRes, resource)),
      readFileSync(join(androidSource, resource)),
    );
  }
  assert.equal(
    readFileSync(join(androidRes, "values", "strings.xml"), "utf8"),
    "unrelated resource",
  );
  writeFileSync(join(androidRes, androidLauncher), "custom Android artwork");
  run("android", "init");

  run("ios", "init");
  assert.equal(
    readFileSync(join(androidRes, androidLauncher), "utf8"),
    "custom Android artwork",
  );
  assert.deepEqual(
    readdirSync(iosIcons).sort(),
    ["Contents.json", ...iosNames].sort(),
  );
  assert.equal(readFileSync(join(iosIcons, "Contents.json"), "utf8"), catalog);
  for (const name of iosNames) {
    assert.deepEqual(
      readFileSync(join(iosIcons, name)),
      readFileSync(join(tauri, "icons", "ios", name)),
    );
  }
  writeFileSync(join(iosIcons, iosNames[0]), "custom iOS artwork");
  run("ios", "init");
  assert.equal(
    readFileSync(join(iosIcons, iosNames[0]), "utf8"),
    "custom iOS artwork",
  );
});

test("mobile help and other subcommands leave launcher assets alone", (t) => {
  const { tauri, run } = mobileFixture(t);
  run("android", "init", "--help");
  run("ios", "init", "-vh");
  run("android", "dev");
  assert.equal(existsSync(join(tauri, "gen")), false);
});

test("failed mobile init leaves the generated stock artwork alone", (t) => {
  const { invoke, androidRes, androidLauncher, stock } = mobileFixture(t);
  const result = invoke(["android", "init"], { DELIDEV_TEST_FAIL_INIT: "1" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Tauri init failed/);
  assert.equal(readFileSync(join(androidRes, androidLauncher), "utf8"), stock);
});

test("successful CLI return without a generated mobile host fails init", (t) => {
  const { tauri, invoke } = mobileFixture(t);
  const result = invoke(["android", "init"], { DELIDEV_TEST_NO_HOST: "1" });
  assert.equal(result.status, 1);
  assert.match(
    result.stderr,
    /Failed to install DeliDev launcher icons for android.*AndroidManifest\.xml/,
  );
  assert.equal(existsSync(join(tauri, "gen")), false);
});

test("missing launcher artwork fails init before replacing stock assets", (t) => {
  const { tauri, invoke, iosIcons, iosNames, stock } = mobileFixture(t);
  const missing = iosNames[1];
  rmSync(join(tauri, "icons", "ios", missing));
  const result = invoke(["ios", "init"]);
  assert.equal(result.status, 1);
  assert.match(
    result.stderr,
    /Failed to install DeliDev launcher icons for ios/,
  );
  assert.ok(result.stderr.includes(missing));
  assert.equal(readFileSync(join(iosIcons, iosNames[0]), "utf8"), stock);
});

test("changes only the CEF arm of the pinned Linux dependency mapping", () => {
  const pinned = `    match self {
      Self::Wry => &[WEBKIT2GTK, GTK],
      // CEF is shipped with the app
      Self::Cef | Self::Other => &[GTK],
    }`;
  assert.equal(
    patchCefLinuxDependencies(pinned),
    `    match self {
      Self::Wry => &[WEBKIT2GTK, GTK],
      Self::Cef => &[LinuxDependency {
        deb_package: "libgtk-4-1",
        library: "libgtk-4.so.1",
      }],
      Self::Other => &[GTK],
    }`,
  );
  for (const source of ["", pinned + pinned]) {
    assert.throws(
      () => patchCefLinuxDependencies(source),
      /Could not locate the pinned CEF Linux dependency mapping/,
    );
  }
});

test(
  "uses the stock CLI for requests outside the Linux backport",
  linuxOnly,
  (t) => {
    const { run } = fixture(t);
    for (const args of [
      ["bundle", "--bundles", "appimage"],
      ["bundle", "--config", embedCef, "--bundles", "dmg"],
      ["bundle", "--config", embedCef, "--bundles"],
      ["build", "--config", embedCef, "--no-bundle"],
      ["build", "--help", "--bundles", "rpm"],
      ["build", "--config", '{"bundle":{"active":false,"cef":null}}'],
      ["build", "--target", "aarch64-linux-android", "--bundles", "rpm"],
      ["bundle", "--target", "x86_64-pc-windows-msvc", "--bundles", "appimage"],
    ]) {
      assert.deepEqual(run(...args), { cli: "stock", args });
    }
  },
);

test(
  "uses the backport without staging notices for shared CEF Debian and RPM",
  linuxOnly,
  (t) => {
    const { run, staging } = fixture(t);
    for (const command of ["build", "bundle"]) {
      for (const selection of [
        [], // Selects Debian from the Linux config.
        ["--bundles", "deb"],
        ["--bundles", "rpm,appimage"],
        ["--config", '{"bundle":{"targets":["rpm"]}}'],
        [
          "--config",
          embedCef,
          "--config",
          '{"bundle":{"cef":{"embed":false}}}',
        ],
      ]) {
        const args = [command, ...selection];
        assert.deepEqual(run(...args), { cli: "patched", args });
        assert.deepEqual(readdirSync(staging), []);
      }
    }
  },
);

test(
  "maps staged notices only into the selected embedded Linux bundles",
  linuxOnly,
  (t) => {
    const { run, staging } = fixture(t);
    const docDir = "usr/share/doc/delidev/cef";
    for (const command of ["build", "bundle"]) {
      for (const [selection, formats] of [
        [["--bundles", "rpm"], ["rpm"]],
        [["--bundles=appimage"], ["appimage"]],
        [["-brpm,appimage"], ["rpm", "appimage"]],
        [
          ["--bundles", "rpm", "appimage"],
          ["rpm", "appimage"],
        ],
        [["--config", '{"bundle":{"targets":"rpm"}}'], ["rpm"]],
        [["--config", '{"bundle":{"targets":["appimage"]}}'], ["appimage"]],
        [
          ["--config", '{"bundle":{"targets":null}}'],
          ["deb", "rpm", "appimage"],
        ],
      ]) {
        const args = [command, "--config", embedCef, ...selection];
        const result = run(...args);
        assert.equal(result.cli, "patched");
        const patch = JSON.parse(
          result.args[result.args.lastIndexOf("--config") + 1],
        );
        assert.match(
          patch.build?.beforeBundleCommand ?? "",
          /cef-notices\.mjs/,
        );
        const linux = patch.bundle.linux;
        assert.deepEqual(
          Object.keys(linux),
          formats.filter((format) => format !== "deb"),
        );
        if (formats.includes("deb")) {
          const deb = JSON.parse(result.args[2]).bundle.linux.deb;
          assert.deepEqual(deb.files, {
            [`/${docDir}/LICENSE.txt`]: Object.values(linux.rpm.files)[0],
            [`/${docDir}/CREDITS.html`]: Object.values(linux.rpm.files)[1],
          });
        } else {
          assert.deepEqual(result.args.slice(0, 3), args.slice(0, 3));
        }
        const sources = [];
        for (const format of Object.keys(linux)) {
          const dir = format === "rpm" ? `/${docDir}` : docDir;
          const { files } = linux[format];
          assert.deepEqual(Object.keys(files), [
            `${dir}/LICENSE.txt`,
            `${dir}/CREDITS.html`,
          ]);
          sources.push(Object.values(files));
        }
        for (const paths of sources) assert.deepEqual(paths, sources[0]);
        assert.deepEqual(readdirSync(staging), []);
      }
    }
  },
);

test(
  "selects notice staging for embedded macOS apps and DMGs",
  unixOnly,
  (t) => {
    const { run } = fixture(t, "darwin");
    for (const args of [
      ["bundle", "--bundles", "dmg"],
      ["bundle", "--config", embedCef, "--bundles"],
      ["bundle", "--config", embedCef, "--bundles", "ios"],
      ["build", "--config", embedCef, "--no-bundle"],
      ["build", "--config", embedCef, "--target", "aarch64-apple-ios"],
      [
        "build",
        "--config",
        embedCef,
        "--config",
        '{"bundle":{"active":false}}',
      ],
    ]) {
      assert.deepEqual(run(...args), { cli: "stock", args });
    }

    for (const args of [
      ["build", "--config", embedCef, "--bundles", "app"],
      ["bundle", "--config", embedCef, "--bundles", "dmg"],
      ["build", "--config", embedCef], // Selects DMG from the macOS config.
    ]) {
      const result = run(...args);
      assert.equal(result.cli, "stock");
      const index = result.args.lastIndexOf("--config");
      assert.deepEqual(
        [...result.args.slice(0, index), ...result.args.slice(index + 2)],
        args,
      );
      const patch = JSON.parse(result.args[index + 1]);
      assert.deepEqual(Object.keys(patch.bundle.macOS.files), [
        "Resources/cef/LICENSE.txt",
        "Resources/cef/CREDITS.html",
      ]);
    }
  },
);

test("preserves caller macOS file mappings and resources", unixOnly, (t) => {
  const { root, run } = fixture(t, "darwin");
  writeFileSync(
    join(root, "src-tauri", "tauri.macos.conf.json"),
    JSON.stringify({
      bundle: { macOS: { files: { "Resources/old.txt": "old.txt" } } },
    }),
  );
  for (const resources of [["existing.txt"], { "existing.txt": "moved.txt" }]) {
    const custom = JSON.stringify({ bundle: { resources } });
    const result = run("bundle", "--config", custom, "--bundles", "app");
    assert.equal(result.args[2], custom);
    const bundle = JSON.parse(
      result.args[result.args.lastIndexOf("--config") + 1],
    ).bundle;
    assert.deepEqual(Object.keys(bundle), ["macOS"]);
    assert.equal(Object.hasOwn(bundle.macOS.files, "Resources/old.txt"), false);
  }
  const clear = '{"bundle":{"macOS":{"files":null}}}';
  const result = run("bundle", "--config", clear, "--bundles", "app");
  const files = JSON.parse(result.args[result.args.lastIndexOf("--config") + 1])
    .bundle.macOS.files;
  assert.equal(files["Resources/old.txt"], null);
});

test(
  "stages direct macOS notices and runs the caller hook in its cwd",
  unixOnly,
  (t) => {
    const { root, staging, invoke } = fixture(t, "darwin");
    const distribution = join(root, "direct CEF");
    const hookCwd = join(root, "caller hook");
    mkdirSync(join(distribution, "Chromium Embedded Framework.framework"), {
      recursive: true,
    });
    mkdirSync(hookCwd);
    const license = Buffer.from("direct license\r\n");
    const credits = Buffer.from("<p>direct credits</p>\n");
    writeFileSync(join(distribution, "LICENSE.txt"), license);
    writeFileSync(join(distribution, "CREDITS.html"), credits);
    const custom = JSON.stringify({
      build: {
        beforeBundleCommand: {
          cwd: hookCwd,
          script:
            'test -f "$DELIDEV_CEF_NOTICE_DIR/LICENSE.txt" && pwd > called',
        },
      },
    });
    const result = invoke(
      ["build", "--config", embedCef, "--config", custom, "--bundles", "app"],
      {
        CEF_PATH: "../direct CEF",
        DELIDEV_TEST_RUN_HOOK: "1",
      },
    );
    assert.equal(result.status, 0, result.stderr);
    const resourceDir = join(
      root,
      "fixture.app",
      "Contents",
      "Resources",
      "cef",
    );
    assert.deepEqual(readFileSync(join(resourceDir, "LICENSE.txt")), license);
    assert.deepEqual(readFileSync(join(resourceDir, "CREDITS.html")), credits);
    assert.equal(
      readFileSync(join(hookCwd, "called"), "utf8").trim(),
      realpathSync(hookCwd),
    );
    assert.deepEqual(readdirSync(staging), []);
  },
);

test(
  "recovers a cached macOS license and fails before packaging on invalid inputs",
  unixOnly,
  (t) => {
    const { root, staging, invoke } = fixture(t, "darwin");
    const home = join(root, "home");
    const cache = join(home, "Library", "Caches", "tauri-cef", "4.5.6");
    const distribution = join(cache, "cef_macos_aarch64");
    mkdirSync(join(distribution, "Chromium Embedded Framework.framework"), {
      recursive: true,
    });
    const credits = Buffer.from("<p>cached credits</p>\n");
    writeFileSync(join(distribution, "CREDITS.html"), credits);
    const archiveStem = "cef_binary_fixture_macosarm64_minimal";
    const member = join(archiveStem, "LICENSE.txt");
    const archive = join(cache, `${archiveStem}.tar.bz2`);
    const license = Buffer.from("archived license\r\n");
    mkdirSync(join(root, archiveStem));
    writeFileSync(join(root, member), license);
    execFileSync("tar", ["-cjf", archive, member], { cwd: root });
    const contents = readFileSync(archive);
    writeFileSync(
      join(distribution, "archive.json"),
      JSON.stringify({
        name: `${archiveStem}.tar.bz2`,
        sha1: createHash("sha1").update(contents).digest("hex"),
      }),
    );
    const args = ["bundle", "--config", embedCef, "--bundles", "dmg"];
    const env = { CEF_PATH: undefined, HOME: home, DELIDEV_TEST_RUN_HOOK: "1" };
    const result = invoke(args, env);
    assert.equal(result.status, 0, result.stderr);
    const app = join(root, "fixture.app");
    const resourceDir = join(app, "Contents", "Resources", "cef");
    assert.deepEqual(readFileSync(join(resourceDir, "LICENSE.txt")), license);
    assert.deepEqual(readFileSync(join(resourceDir, "CREDITS.html")), credits);
    assert.deepEqual(readdirSync(staging), []);
    rmSync(app, { recursive: true });

    writeFileSync(archive, "corrupt");
    const corrupt = invoke(args, env);
    assert.equal(corrupt.status, 1);
    assert.match(
      corrupt.stderr,
      /CEF archive checksum mismatch.*cef_binary_fixture/,
    );
    assert.equal(existsSync(app), false);
    assert.deepEqual(readdirSync(staging), []);

    rmSync(archive);
    const missingArchive = invoke(args, env);
    assert.equal(missingArchive.status, 1);
    assert.match(missingArchive.stderr, /ENOENT.*cef_binary_fixture/);
    assert.equal(existsSync(app), false);
    assert.deepEqual(readdirSync(staging), []);

    writeFileSync(archive, contents);
    rmSync(join(distribution, "CREDITS.html"));
    const missingCredits = invoke(args, env);
    assert.equal(missingCredits.status, 1);
    assert.match(missingCredits.stderr, /ENOENT.*CREDITS\.html/);
    assert.equal(existsSync(app), false);
    assert.deepEqual(readdirSync(staging), []);
  },
);

test(
  "applies platform and CLI bundle overrides before selecting the backport",
  linuxOnly,
  (t) => {
    const { run } = fixture(t);
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
        '{"bundle":{"targets":["deb","rpm","appimage"]}}',
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
        arg.includes("cef-notices.mjs"),
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
    const { run } = fixture(t);
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
    assert.match(build.beforeBundleCommand, /cef-notices\.mjs/);

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

const debianArgs = (command) => [
  command,
  "--config",
  embedCef,
  "--bundles",
  "deb",
];

test(
  "cleans Debian notice staging on CLI exit and spawn failure",
  linuxOnly,
  (t) => {
    const { root, staging, invoke } = fixture(t);
    const cli = join(root, "cargo-tauri");
    writeFileSync(
      cli,
      `#!/usr/bin/env node
import { existsSync } from "node:fs";
if (!existsSync(process.env.DELIDEV_CEF_NOTICE_DIR)) process.exit(99);
process.exitCode = Number(process.env.FIXTURE_STATUS);
`,
    );
    for (const code of [0, 2]) {
      const result = invoke(debianArgs("build"), {
        FIXTURE_STATUS: String(code),
      });
      assert.equal(result.status, code, result.stderr);
      assert.deepEqual(readdirSync(staging), []);
    }

    rmSync(cli);
    const result = invoke(debianArgs("bundle"));
    assert.equal(result.status, 1);
    assert.match(result.stderr, /spawn.*cargo-tauri ENOENT/);
    assert.deepEqual(readdirSync(staging), []);
  },
);

for (const [command, signal, status] of [
  ["build", "SIGTERM", 143],
  ["bundle", "SIGTERM", 143],
  ["bundle", "SIGINT", 130],
]) {
  test(
    `waits for Debian ${command} to handle ${signal} before removing notices`,
    { ...linuxOnly, timeout: 5000 },
    async (t) => {
      const { root, staging, start } = fixture(t);
      writeFileSync(
        join(root, "cargo-tauri"),
        `#!/usr/bin/env node
import { existsSync } from "node:fs";
const noticeDir = process.env.DELIDEV_CEF_NOTICE_DIR;
let deliveries = 0;
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    deliveries++;
    console.log(JSON.stringify({ signal, staged: existsSync(noticeDir) }));
  });
}
process.stdin.resume();
process.stdin.on("end", () => console.log(JSON.stringify({ deliveries })));
console.log(JSON.stringify({ noticeDir }));
`,
      );
      const wrapper = start(debianArgs(command));
      const exited = once(wrapper, "exit");
      const closed = once(wrapper, "close");
      t.after(async () => {
        // Kill the fixture group even if the wrapper exits and leaves its CLI running.
        if (wrapper.exitCode !== status) {
          try {
            process.kill(-wrapper.pid, "SIGKILL");
          } catch (error) {
            if (error.code !== "ESRCH") throw error;
          }
        }
        await closed;
      });

      const lines = createInterface({ input: wrapper.stdout })[
        Symbol.asyncIterator
      ]();
      const { noticeDir } = JSON.parse((await lines.next()).value);
      assert.ok(existsSync(noticeDir));
      // A terminal Ctrl-C goes to the entire foreground process group.
      process.kill(signal === "SIGINT" ? -wrapper.pid : wrapper.pid, signal);
      const shutdown = await Promise.race([
        lines.next(),
        exited.then(([code, endingSignal]) => {
          throw new Error(`Wrapper exited early: ${code ?? endingSignal}`);
        }),
      ]);
      assert.deepEqual(JSON.parse(shutdown.value), { signal, staged: true });
      assert.equal(wrapper.exitCode, null);
      assert.ok(existsSync(noticeDir));
      wrapper.stdin.end();
      assert.deepEqual(JSON.parse((await lines.next()).value), {
        deliveries: 1,
      });
      assert.deepEqual(await closed, [status, null]);
      assert.deepEqual(readdirSync(staging), []);
    },
  );
}
