// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import test from "node:test";

import { resolveChildEnv } from "./dev.mjs";

// Native Windows check of the environment resolution: it starts a real child
// with the resolved environment, so Node's own handling of names that differ
// only by case applies. It is skipped on other platforms, where the child
// would keep both spellings and the check would say nothing about Windows.
// The platform-parameterized fixture tests are in dev-env.test.mjs.
const DefaultCorsOrigins = [
  "https://accounts.delino.io",
  "http://127.0.0.1:13100",
  "https://accounts.delino.localhost",
  "http://accounts.delino.localhost",
].join(",");
const defaults = {
  CORS_ORIGINS: DefaultCorsOrigins,
  WEB_ORIGIN: "https://accounts.delino.localhost",
};
const explicit = {
  CORS_ORIGINS: "http://127.0.0.1:14100",
  WEB_ORIGIN: "http://127.0.0.1:14100",
};
const names = Object.keys(defaults);
const skip = process.platform !== "win32";

const mixedCase = (name) =>
  name
    .split("_")
    .map((part) => part[0] + part.slice(1).toLowerCase())
    .join("_");

// The child prints what a server reads through uppercase lookups and how many
// entries of each setting its environment holds.
const probe = `
const names = ${JSON.stringify(names)};
const out = {};
for (const name of names) {
  out[name] = {
    value: process.env[name],
    entries: Object.keys(process.env).filter((key) => key.toUpperCase() === name).length,
  };
}
console.log(JSON.stringify(out));
`;

function childView(base) {
  const result = spawnSync(process.execPath, ["-e", probe], {
    env: resolveChildEnv(base),
    encoding: "utf8",
    shell: false,
    timeout: 30_000,
  });
  assert.equal(result.status, 0, String(result.stderr));
  return JSON.parse(result.stdout);
}

// Starts from the real environment so SystemRoot and Path reach the child.
function baseWith(entries) {
  const base = { ...process.env };
  for (const key of Object.keys(base)) {
    if (names.includes(key.toUpperCase())) delete base[key];
  }
  return Object.assign(base, entries);
}

for (const [label, spell] of [
  ["lowercase", (name) => name.toLowerCase()],
  ["mixed-case", mixedCase],
]) {
  test(`the child reads explicit ${label} values once`, { skip }, () => {
    const entries = {};
    for (const name of names) entries[spell(name)] = explicit[name];
    const view = childView(baseWith(entries));
    for (const name of names) {
      assert.deepEqual(view[name], { value: explicit[name], entries: 1 }, name);
    }
  });
}

test("the child reads each default once", { skip }, () => {
  for (const blank of [undefined, "", "   "]) {
    const entries = {};
    if (blank !== undefined) {
      for (const name of names) entries[name.toLowerCase()] = blank;
    }
    const view = childView(baseWith(entries));
    for (const name of names) {
      assert.deepEqual(view[name], { value: defaults[name], entries: 1 }, name);
    }
  }
});
