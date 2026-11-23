// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import test from "node:test";
import {
  CommandLineTooLongError,
  runFormatter,
  splitBatches,
  windowsQuotedLength,
} from "./format-changed.mjs";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const require = createRequire(import.meta.url);

// The splitter is pure and platform independent: the tests pass the limit
// explicitly, so they run (and mean the same) on every operating system.
const WindowsLimit = 32767;
const executable = "node";
const fixedArgs = ["rs.js", "fmt", "--write"];
// `node rs.js fmt --write` plus one NUL.
const fixedLength = executable.length + fixedArgs.join(" ").length + 1 + 1;

test("windowsQuotedLength follows the libuv quoting rules", () => {
  const cases = [
    ["", '""'],
    ["plain", "plain"],
    ["back\\slash", "back\\slash"],
    ["trailing\\", "trailing\\"],
    ["with space", '"with space"'],
    ["with\ttab", '"with\ttab"'],
    ['hello"world', '"hello\\"world"'],
    ['hello""world', '"hello\\"\\"world"'],
    ['hello\\"world', '"hello\\\\\\"world"'],
    ['hello\\\\"world', '"hello\\\\\\\\\\"world"'],
    ["hello world\\", '"hello world\\\\"'],
    ["a b\\c", '"a b\\c"'],
    ["a b\\\\", '"a b\\\\\\\\"'],
    ["한글 name", '"한글 name"'],
    // An astral character is two UTF-16 code units, the unit of the limit.
    ["\u{1F600} x", '"\u{1F600} x"'],
  ];
  for (const [arg, serialized] of cases) {
    assert.equal(
      windowsQuotedLength(arg),
      serialized.length,
      JSON.stringify(arg),
    );
  }
  assert.equal(windowsQuotedLength("\u{1F600}"), 2);
});

test("splitBatches keeps the 50-path cap and preserves order without a length limit", () => {
  const names = Array.from(
    { length: 120 },
    (_, index) => `dir/file-${index}.md`,
  );
  const batches = splitBatches(executable, fixedArgs, names, {
    limit: Infinity,
  });
  assert.deepEqual(
    batches.map((batch) => batch.length),
    [50, 50, 20],
  );
  assert.deepEqual(batches.flat(), names);
  assert.deepEqual(splitBatches(executable, fixedArgs, []), []);
});

test("splitBatches limits the length on win32 only, whatever the host platform", () => {
  // Three 230-character directories make every argument about 700 units, so
  // 50 paths are well over the 32,767 limit and need several batches on win32.
  const component = "d".repeat(230);
  const names = Array.from(
    { length: 50 },
    (_, index) => `${component}/${component}/${component}/${index}.md`,
  );
  const total = names.reduce(
    (sum, name) => sum + 1 + windowsQuotedLength(`./${name}`),
    0,
  );
  assert.ok(total > WindowsLimit);
  const windows = splitBatches(executable, fixedArgs, names, {
    platform: "win32",
  });
  assert.ok(windows.length > 1);
  assert.deepEqual(windows.flat(), names);
  for (const platform of ["linux", "darwin"]) {
    assert.deepEqual(splitBatches(executable, fixedArgs, names, { platform }), [
      names,
    ]);
  }
  // An explicit limit still wins over the platform default.
  assert.ok(
    splitBatches(executable, fixedArgs, names, {
      platform: "linux",
      limit: WindowsLimit,
    }).length > 1,
  );
});

test("splitBatches splits 50 long paths by serialized length at the Windows limit", () => {
  const component = "x".repeat(230);
  const names = Array.from(
    { length: 50 },
    (_, index) => `${component}/${component}/${component}/${index}.md`,
  );
  const batches = splitBatches(executable, fixedArgs, names, {
    limit: WindowsLimit,
  });
  assert.ok(batches.length > 1);
  assert.deepEqual(batches.flat(), names);
  const lengths = batches.map(
    (batch) =>
      fixedLength +
      batch.reduce(
        (total, name) => total + 1 + windowsQuotedLength(`./${name}`),
        0,
      ),
  );
  for (const length of lengths) assert.ok(length <= WindowsLimit);
  // Every batch is as full as possible: the next path would not have fit.
  for (let index = 0; index < batches.length - 1; index++) {
    const next = names[batches.slice(0, index + 1).flat().length];
    assert.ok(
      lengths[index] + 1 + windowsQuotedLength(`./${next}`) > WindowsLimit ||
        batches[index].length === 50,
    );
  }
});

test("splitBatches accepts a command line of exactly the limit and splits one unit over", () => {
  const names = ["aaaa.md", "bbbb.md", "cccc.md"];
  const one = 1 + windowsQuotedLength("./aaaa.md");
  const exact = fixedLength + 2 * one;
  assert.deepEqual(
    splitBatches(executable, fixedArgs, names, { limit: exact }),
    [["aaaa.md", "bbbb.md"], ["cccc.md"]],
  );
  assert.deepEqual(
    splitBatches(executable, fixedArgs, names, { limit: exact - 1 }),
    [["aaaa.md"], ["bbbb.md"], ["cccc.md"]],
  );
});

test("splitBatches counts quoting, the executable and the fixed arguments", () => {
  // Spaces and quotes add quote marks and backslashes to the serialized form.
  const spaced = ["a b.md", 'q"uote.md', "trail dir\\x.md"];
  const spacedLength = spaced.reduce(
    (total, name) => total + 1 + windowsQuotedLength(`./${name}`),
    0,
  );
  const plainLength = spaced.reduce(
    (total, name) => total + 1 + name.length + 2,
    0,
  );
  assert.ok(spacedLength > plainLength);
  const limit = fixedLength + spacedLength;
  assert.deepEqual(splitBatches(executable, fixedArgs, spaced, { limit }), [
    spaced,
  ]);
  assert.equal(
    splitBatches(executable, fixedArgs, spaced, { limit: limit - 1 }).length,
    2,
  );
  // An executable path with a space is quoted, which adds to the fixed part.
  const longExecutable = `C:\\Program Files\\${"n".repeat(100)}\\node.exe`;
  const extra =
    windowsQuotedLength(longExecutable) - windowsQuotedLength(executable);
  assert.ok(extra > 100);
  assert.deepEqual(
    splitBatches(longExecutable, fixedArgs, spaced, { limit: limit + extra }),
    [spaced],
  );
  assert.equal(
    splitBatches(longExecutable, fixedArgs, spaced, {
      limit: limit + extra - 1,
    }).length,
    2,
  );
});

test("splitBatches names a path that cannot fit even alone", () => {
  const names = ["ok.md", `${"y".repeat(100)}.md`, "after.md"];
  const limit = fixedLength + 1 + windowsQuotedLength("./ok.md");
  assert.throws(
    () => splitBatches(executable, fixedArgs, names, { limit }),
    (error) =>
      error instanceof CommandLineTooLongError &&
      error.path === names[1] &&
      error.limit === limit &&
      error.message.includes(names[1]),
  );
  // The path that fits exactly alone is accepted.
  assert.deepEqual(splitBatches(executable, fixedArgs, ["ok.md"], { limit }), [
    ["ok.md"],
  ]);
});

// Process boundary: the real Rstack runs in a scratch repository with a small
// injected command-line limit so that several batches are needed.
function scratchRepository(t) {
  const parent = mkdtempSync(join(tmpdir(), "rstack-batches-"));
  t.after(() => rmSync(parent, { recursive: true, force: true }));
  const env = isolateGitEnvironment({ ...process.env });
  const git = (...args) =>
    execFileSync("git", args, { cwd: parent, env, encoding: "utf8" }).trim();
  git("init", "--quiet");
  writeFileSync(join(parent, "package.json"), '{"type":"module"}\n');
  copyFileSync(
    join(root, "rstack.config.ts"),
    join(parent, "rstack.config.ts"),
  );
  symlinkSync(
    join(root, "node_modules"),
    join(parent, "node_modules"),
    process.platform === "win32" ? "junction" : "dir",
  );
  writeFileSync(join(parent, ".gitignore"), "node_modules\n");
  const write = (name, text) => {
    mkdirSync(dirname(join(parent, name)), { recursive: true });
    writeFileSync(join(parent, name), text);
  };
  return {
    cwd: parent,
    write,
    read: (name) => readFileSync(join(parent, name), "utf8"),
  };
}

// The command line that `runFormatter` serializes for the given mode and paths,
// measured with the same quoting rules as the splitter.
function commandLineLength(mode, names) {
  const manifestPath = require.resolve("rstack/package.json");
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  const entry = join(dirname(manifestPath), manifest.bin.rs);
  const args = [
    process.execPath,
    entry,
    "fmt",
    `--${mode}`,
    "--no-cache",
    "--no-error-on-unmatched-pattern",
    ...names.map((name) => `./${name}`),
  ];
  // One separator or NUL after every argument.
  return args.reduce((total, arg) => total + windowsQuotedLength(arg) + 1, 0);
}

test("runFormatter processes every path once, in order, across length-split batches", async (t) => {
  const f = scratchRepository(t);
  const names = Array.from(
    { length: 6 },
    (_, index) => `dir ${index}/note-${index}.ts`,
  );
  for (const name of names) f.write(name, "const value={answer:42}\n");
  const formatted = "const value = { answer: 42 };\n";
  const seen = [];
  // Room for exactly two of these paths in write mode (the longer mode name),
  // so a three-path batch could never be built and every run needs batches.
  const options = {
    commandLineLimit: commandLineLength("write", names.slice(0, 2)),
    onBatch: (batch) => seen.push(batch),
  };
  assert.equal(await runFormatter(f.cwd, names, "check", options), 1);
  assert.ok(seen.length >= 3);
  assert.ok(seen.every((batch) => batch.length <= 2));
  assert.deepEqual(seen.flat(), names);
  for (const name of names)
    assert.equal(f.read(name), "const value={answer:42}\n");
  seen.length = 0;
  assert.equal(await runFormatter(f.cwd, names, "write", options), 0);
  assert.deepEqual(seen, [
    [names[0], names[1]],
    [names[2], names[3]],
    [names[4], names[5]],
  ]);
  for (const name of names) assert.equal(f.read(name), formatted);
  seen.length = 0;
  assert.equal(await runFormatter(f.cwd, names, "check", options), 0);
  assert.deepEqual(seen.flat(), names);
});

test("runFormatter reports an unaddressable path by name before it starts any batch", async (t) => {
  const f = scratchRepository(t);
  const long = `${"z".repeat(200)}.ts`;
  f.write("short.ts", "const value={answer:42}\n");
  f.write(long, "const value={answer:42}\n");
  const seen = [];
  await assert.rejects(
    runFormatter(f.cwd, ["short.ts", long], "write", {
      commandLineLimit: commandLineLength("write", ["short.ts"]),
      onBatch: (batch) => seen.push(batch),
    }),
    (error) => error instanceof CommandLineTooLongError && error.path === long,
  );
  assert.deepEqual(seen, []);
  // Nothing was rewritten, not even the path that did fit.
  assert.equal(f.read("short.ts"), "const value={answer:42}\n");
});
