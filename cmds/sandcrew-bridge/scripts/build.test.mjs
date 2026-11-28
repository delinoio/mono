// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { buildPlan, parseArgs, targets } from "./build.mjs";

const packageDir = join(dirname(fileURLToPath(import.meta.url)), "..");

test("the build targets equal the pinned release targets", () => {
  const pins = JSON.parse(
    readFileSync(join(packageDir, "internal/pins/pins.json"), "utf8"),
  );
  const pinned = Object.keys(pins.runtimes.node.targets).sort();
  assert.deepEqual(targets.map((t) => `${t.goos}-${t.goarch}`).sort(), pinned);
});

test("the plan cross-compiles static binaries with the version", () => {
  const plan = buildPlan({ out: "/out", version: "1.2.3", commit: "abc" });
  assert.equal(plan.length, 5);
  const windows = plan.find((step) => step.target === "windows-arm64");
  assert.equal(
    windows.output,
    join("/out", "windows-arm64", "sandcrew-bridge.exe"),
  );
  assert.deepEqual(windows.env, {
    CGO_ENABLED: "0",
    GOOS: "windows",
    GOARCH: "arm64",
  });
  assert.ok(windows.args.includes("-trimpath"));
  const ldflags = windows.args[windows.args.indexOf("-ldflags") + 1];
  assert.match(ldflags, /internal\/version\.Version=1\.2\.3 /);
  assert.match(ldflags, /internal\/version\.Commit=abc$/);
});

test("arguments and environment set the version", () => {
  assert.equal(
    parseArgs([], { SANDCREW_BRIDGE_VERSION: "2.0.0" }).version,
    "2.0.0",
  );
  assert.equal(parseArgs(["--version", "3.0.0"], {}).version, "3.0.0");
  assert.throws(() => parseArgs(["--version", "1 -X x=y"], {}));
  assert.throws(() => parseArgs(["--unknown", "x"], {}));
  assert.throws(() => parseArgs(["--commit"], {}));
});
