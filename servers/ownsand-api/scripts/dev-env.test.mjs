// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import test from "node:test";

import { resolveChildEnv } from "./dev.mjs";

// These tests call the environment resolution of the launcher directly with
// both platform rules, so they spawn nothing and run on every platform. The
// launcher process tests in dev.test.mjs are skipped on Windows. The native
// Windows check is in dev-env-windows.test.mjs.
const DefaultCorsOrigins = [
  "https://ownsand.delino.io",
  "http://127.0.0.1:13300",
  "https://ownsand.delino.localhost",
  "http://ownsand.delino.localhost",
].join(",");
const defaults = {
  CORS_ORIGINS: DefaultCorsOrigins,
  ALLOW_DIRECT_CLIENT_ADDRESS: "true",
};
const explicit = {
  CORS_ORIGINS: "http://127.0.0.1:14100",
  ALLOW_DIRECT_CLIENT_ADDRESS: "false",
};
const names = Object.keys(defaults);
const placeholderDatabase = "postgres://placeholder@127.0.0.1:1/placeholder";

// Entries of `env` whose name matches `name` without regard to case.
const spellings = (env, name) =>
  Object.keys(env).filter((key) => key.toUpperCase() === name);

const mixedCase = (name) =>
  name
    .split("_")
    .map((part) => part[0] + part.slice(1).toLowerCase())
    .join("_");

test("Windows keeps an explicit lowercase or mixed-case value once", () => {
  for (const spell of [(name) => name.toLowerCase(), mixedCase]) {
    const base = { DATABASE_URL: placeholderDatabase, Path: "keep" };
    for (const name of names) base[spell(name)] = explicit[name];
    const env = resolveChildEnv(base, "win32");
    for (const name of names) {
      assert.deepEqual(spellings(env, name), [spell(name)], name);
      assert.equal(env[spell(name)], explicit[name], name);
    }
    assert.equal(env.DATABASE_URL, placeholderDatabase);
    assert.equal(env.Path, "keep");
  }
});

test("Windows applies each default once when the setting is absent", () => {
  const env = resolveChildEnv({ DATABASE_URL: placeholderDatabase }, "win32");
  for (const name of names) {
    assert.deepEqual(spellings(env, name), [name], name);
    assert.equal(env[name], defaults[name], name);
  }
});

test("Windows replaces a blank value in the same entry", () => {
  for (const blank of ["", " ", " \t\n\v\f\r "]) {
    for (const spell of [
      (name) => name,
      (name) => name.toLowerCase(),
      mixedCase,
    ]) {
      const base = {};
      for (const name of names) base[spell(name)] = blank;
      const env = resolveChildEnv(base, "win32");
      for (const name of names) {
        assert.deepEqual(spellings(env, name), [spell(name)], name);
        assert.equal(env[spell(name)], defaults[name], name);
      }
    }
  }
});

test("Windows treats a value of only Unicode white space as set", () => {
  const base = {};
  for (const name of names) base[name.toLowerCase()] = "\u00a0";
  const env = resolveChildEnv(base, "win32");
  for (const name of names) {
    assert.deepEqual(spellings(env, name), [name.toLowerCase()], name);
    assert.equal(env[name.toLowerCase()], "\u00a0", name);
  }
});

test("Windows keeps one nonempty alias when several spellings exist", () => {
  for (const name of names) {
    const env = resolveChildEnv(
      {
        [name]: "",
        [name.toLowerCase()]: explicit[name],
        [mixedCase(name)]: "   ",
      },
      "win32",
    );
    assert.deepEqual(spellings(env, name), [name.toLowerCase()], name);
    assert.equal(env[name.toLowerCase()], explicit[name], name);
  }
});

test("POSIX keeps uppercase overrides", () => {
  const env = resolveChildEnv({ ...explicit }, "linux");
  for (const name of names) assert.equal(env[name], explicit[name], name);
  assert.deepEqual(Object.keys(env).sort(), [...names].sort());
});

test("POSIX applies the defaults to unset, empty and blank values", () => {
  const unset = resolveChildEnv({}, "linux");
  for (const name of names) assert.equal(unset[name], defaults[name], name);
  for (const blank of ["", "   ", "\t\r\n"]) {
    const base = {};
    for (const name of names) base[name] = blank;
    const env = resolveChildEnv(base, "linux");
    for (const name of names) assert.equal(env[name], defaults[name], name);
  }
});

test("POSIX keeps differently cased names as distinct variables", () => {
  for (const spell of [(name) => name.toLowerCase(), mixedCase]) {
    const base = { DATABASE_URL: placeholderDatabase };
    for (const name of names) base[spell(name)] = explicit[name];
    const env = resolveChildEnv(base, "linux");
    for (const name of names) {
      assert.equal(env[spell(name)], explicit[name], name);
      assert.equal(env[name], defaults[name], name);
      assert.deepEqual(spellings(env, name).sort(), [name, spell(name)].sort());
    }
    assert.equal(env.DATABASE_URL, placeholderDatabase);
  }
});

test("the base environment is not modified", () => {
  for (const platform of ["win32", "linux"]) {
    const base = { cors_origins: " ", UNRELATED: "1" };
    resolveChildEnv(base, platform);
    assert.deepEqual(base, { cors_origins: " ", UNRELATED: "1" });
  }
});
