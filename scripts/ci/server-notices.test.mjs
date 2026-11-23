// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for `scripts/ci/server-notices.sh`, the third-party license bundle
// of the server container images, against a temporary Go module whose
// dependencies are local `replace` directories (no network, no module cache).
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const script = fileURLToPath(new URL("./server-notices.sh", import.meta.url));
const hasTool = (name) =>
  spawnSync("sh", ["-c", `command -v ${name}`]).status === 0;
const skip =
  process.platform === "win32"
    ? "the server image scripts run on Linux"
    : !hasTool("go")
      ? "go is not installed"
      : false;

function write(path, content) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

// A module `example.com/app` whose command imports `example.com/lib`
// everywhere, `example.com/windowsonly` only on Windows and
// `example.com/cgoonly` only with CGO. The last two have no license file, so
// the bundle succeeds only when the helper builds for linux/amd64 with CGO
// disabled whatever the caller's environment says.
function fixture(options = {}) {
  const dir = mkdtempSync(join(tmpdir(), "server-notices-"));
  write(
    join(dir, "go.mod"),
    [
      "module example.com/app",
      "",
      "go 1.21",
      "",
      "require (",
      "\texample.com/lib v1.2.3",
      "\texample.com/windowsonly v0.0.1",
      "\texample.com/cgoonly v0.0.1",
      ")",
      "",
      "replace example.com/lib => ./lib",
      "replace example.com/windowsonly => ./windowsonly",
      "replace example.com/cgoonly => ./cgoonly",
      "",
    ].join("\n"),
  );
  write(
    join(dir, "cmd/app/main.go"),
    [
      "package main",
      "",
      'import _ "example.com/lib"',
      "",
      "func main() {}",
      "",
    ].join("\n"),
  );
  write(
    join(dir, "cmd/app/windows.go"),
    '//go:build windows\n\npackage main\n\nimport _ "example.com/windowsonly"\n',
  );
  write(
    join(dir, "cmd/app/cgo.go"),
    '//go:build cgo\n\npackage main\n\nimport _ "example.com/cgoonly"\n',
  );
  for (const name of ["lib", "windowsonly", "cgoonly"]) {
    write(join(dir, name, "go.mod"), `module example.com/${name}\n\ngo 1.21\n`);
    write(join(dir, name, `${name}.go`), `package ${name}\n`);
  }
  if (options.license !== false) {
    write(join(dir, "lib/LICENSE"), "Fixture license text\n");
    write(join(dir, "lib/NOTICE.md"), "Fixture notice text\n");
    // Beyond the collector's maximum depth: not part of the bundle.
    write(join(dir, "lib/a/b/LICENSE"), "Too deep license text\n");
  }
  return dir;
}

function run(dir, args, env = {}) {
  return spawnSync("bash", [script, ...args], {
    cwd: dir,
    encoding: "utf8",
    env: { ...process.env, GOTOOLCHAIN: "local", GOFLAGS: "", ...env },
  });
}

test(
  "writes the license files of the linked modules for linux/amd64 without CGO",
  { skip },
  (t) => {
    const dir = fixture();
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    const out = join(dir, "out/THIRD-PARTY-LICENSES.txt");
    // The caller's environment must not change which modules link.
    const result = run(dir, ["app", "./cmd/app", out], {
      GOOS: "windows",
      GOARCH: "arm64",
      CGO_ENABLED: "1",
    });
    assert.equal(result.status, 0, result.stderr);
    const bundle = readFileSync(out, "utf8");
    assert.match(bundle, /^Third-party licenses of app\n/);
    assert.match(bundle, /example\.com\/lib v1\.2\.3: LICENSE\n/);
    assert.match(bundle, /example\.com\/lib v1\.2\.3: NOTICE\.md\n/);
    assert.match(bundle, /Fixture license text/);
    assert.match(bundle, /Fixture notice text/);
    assert.doesNotMatch(bundle, /Too deep license text/);
    assert.doesNotMatch(bundle, /windowsonly|cgoonly/);
    assert.match(result.stderr, /licenses of 1 third-party modules of app/);
  },
);

test(
  "fails with the module name when a linked module has no license file",
  { skip },
  (t) => {
    const dir = fixture({ license: false });
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    const out = join(dir, "out/THIRD-PARTY-LICENSES.txt");
    const result = run(dir, ["app", "./cmd/app", out]);
    assert.notEqual(result.status, 0);
    assert.match(
      result.stderr,
      /::error::Module example\.com\/lib v1\.2\.3 that app links has no license file in /,
    );
    assert.equal(existsSync(out), false, "no partial bundle is left behind");
  },
);

test("fails when the dependency discovery fails", { skip }, (t) => {
  const dir = fixture();
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const out = join(dir, "out/THIRD-PARTY-LICENSES.txt");
  const result = run(dir, ["app", "./cmd/missing", out]);
  assert.notEqual(result.status, 0);
  assert.match(
    result.stderr,
    /::error::Cannot list the Go modules that app links/,
  );
  assert.equal(existsSync(out), false);
});

test("fails when the package links no third-party module", { skip }, (t) => {
  const dir = fixture();
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  write(join(dir, "cmd/plain/main.go"), "package main\n\nfunc main() {}\n");
  const out = join(dir, "out/THIRD-PARTY-LICENSES.txt");
  const result = run(dir, ["app", "./cmd/plain", out]);
  assert.notEqual(result.status, 0);
  assert.match(
    result.stderr,
    /::error::The Go package \.\/cmd\/plain of app links no third-party module/,
  );
  assert.equal(existsSync(out), false);
});

test("rejects a wrong argument count", { skip }, (t) => {
  const dir = fixture();
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const result = run(dir, ["app"]);
  assert.equal(result.status, 2);
  assert.match(result.stderr, /usage:/);
});
