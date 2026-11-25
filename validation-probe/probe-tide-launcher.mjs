import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const launcher = resolve(process.argv[2] ?? "servers/tide-api/scripts/dev.mjs");
const windows = process.platform === "win32";
const defaults = {
  ALLOW_DIRECT_CLIENT_ADDRESS: "true",
  CORS_ORIGINS:
    "https://tide.delino.io,http://127.0.0.1:13400,https://tide.delino.localhost,http://tide.delino.localhost",
  METRICS_ADDR: "127.0.0.1:19400",
};
const explicit = {
  ALLOW_DIRECT_CLIENT_ADDRESS: "false",
  CORS_ORIGINS: "https://explicit.example",
  METRICS_ADDR: "127.0.0.1:19999",
};
const recase = (settings, spelling, value) =>
  Object.fromEntries(
    Object.entries(settings).map(([name, setting]) => [
      spelling(name),
      value ?? setting,
    ]),
  );
const scratch = mkdtempSync(
  join(dirname(fileURLToPath(import.meta.url)), "launcher-probe-"),
);
try {
  const fakeDir = join(scratch, "bin");
  mkdirSync(fakeDir);
  const source = join(scratch, "main.go");
  writeFileSync(
    source,
    `package main

import (
  "encoding/json"
  "os"
  "strings"
)

func main() {
  phase := "server"
  if len(os.Args) > 1 {
    if os.Args[1] != "build" || os.Args[3] != "-o" {
      panic("unexpected build arguments")
    }
    phase = "build"
    self, err := os.Executable()
    if err != nil {
      panic(err)
    }
    binary, err := os.ReadFile(self)
    if err != nil {
      panic(err)
    }
    if err := os.WriteFile(os.Args[4], binary, 0755); err != nil {
      panic(err)
    }
  }

  settings := map[string]string{}
  for _, entry := range os.Environ() {
    name, value, _ := strings.Cut(entry, "=")
    switch strings.ToUpper(name) {
    case "ALLOW_DIRECT_CLIENT_ADDRESS", "CORS_ORIGINS", "METRICS_ADDR":
      settings[name] = value
    }
  }
  if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": phase, "settings": settings}); err != nil {
    panic(err)
  }
}
`,
  );
  const goVersion = execFileSync("go", ["version"], {
    encoding: "utf8",
  }).trim();
  execFileSync(
    "go",
    ["build", "-o", join(fakeDir, windows ? "go.exe" : "go"), source],
    {
      cwd: scratch,
      env: { ...process.env, TMPDIR: scratch, TMP: scratch, TEMP: scratch },
      stdio: "inherit",
    },
  );

  const cleanEnv = { ...process.env };
  for (const key of Object.keys(cleanEnv)) {
    if (
      [
        ...Object.keys(defaults),
        "DATABASE_URL",
        "PATH",
        "TMP",
        "TEMP",
        "TMPDIR",
      ].includes(key.toUpperCase())
    ) {
      delete cleanEnv[key];
    }
  }
  Object.assign(cleanEnv, {
    DATABASE_URL: "postgres://launcher-probe",
    PATH: `${fakeDir}${delimiter}${process.env.PATH}`,
    TMPDIR: scratch,
    TMP: scratch,
    TEMP: scratch,
  });
  console.log(
    JSON.stringify({
      platform: process.platform,
      node: process.version,
      go: goVersion,
    }),
  );

  function check(label, settings, expected) {
    const output = execFileSync(process.execPath, [launcher], {
      env: { ...cleanEnv, ...settings },
      encoding: "utf8",
      timeout: 30_000,
    }).trim();
    assert.deepEqual(
      output.split("\n").map((line) => JSON.parse(line)),
      [
        { phase: "build", settings: expected },
        { phase: "server", settings: expected },
      ],
      label,
    );
    console.log(
      JSON.stringify({ label, phases: ["build", "server"], status: "passed" }),
    );
  }

  for (const [label, spelling] of [
    ["uppercase", (name) => name],
    ["lowercase", (name) => name.toLowerCase()],
    ["mixed case", (name) => name[0] + name.slice(1).toLowerCase()],
  ]) {
    const overrides = recase(explicit, spelling);
    const blanks = recase(explicit, spelling, " \t\n\v\f\r");
    const defaultSpelling = recase(defaults, spelling);
    check(
      `explicit ${label}`,
      overrides,
      windows ? overrides : { ...defaults, ...overrides },
    );
    check(
      `blank ${label}`,
      blanks,
      windows ? defaultSpelling : { ...blanks, ...defaults },
    );
  }
  check("missing all settings", {}, defaults);
} finally {
  rmSync(scratch, { recursive: true, force: true });
}
