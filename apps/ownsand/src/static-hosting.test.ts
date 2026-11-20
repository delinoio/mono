// SPDX-License-Identifier: Apache-2.0
import { existsSync, readFileSync } from "node:fs";
import { describe, expect, test } from "@rstest/core";

// Cloudflare Pages serves the single-page-application fallback only while the
// build output has no top-level `404.html`, and it ignores a `_redirects`
// splat rule that rewrites to `/index.html` as an infinite loop. These tests
// guard the build input (`public/`, which Rsbuild copies to the output root).
const publicDirectory = new URL("../public/", import.meta.url);

/** Returns true when a `_redirects` line is a splat rule targeting the index document. */
function isSplatToIndexRule(line: string): boolean {
  const [source, destination] = line.trim().split(/\s+/);
  if (source === undefined || destination === undefined) {
    return false;
  }
  return source.endsWith("*") && /^\/index(\.html)?$/.test(destination);
}

describe("Cloudflare Pages static hosting input", () => {
  test("does not ship a top-level 404.html that disables the SPA fallback", () => {
    expect(existsSync(new URL("404.html", publicDirectory))).toBe(false);
  });

  test("does not ship a _redirects splat rule that rewrites to the index document", () => {
    const redirectsUrl = new URL("_redirects", publicDirectory);
    if (!existsSync(redirectsUrl)) {
      return;
    }
    const rules = readFileSync(redirectsUrl, "utf8")
      .split("\n")
      .filter((line) => line.trim() !== "" && !line.trim().startsWith("#"));
    expect(rules.filter(isSplatToIndexRule)).toEqual([]);
  });

  test("does not emit a 404.html into an existing build output", () => {
    // `dist` exists only after a build; the build-input checks above cover the
    // clean-checkout case, and this one covers a built checkout.
    const distDirectory = new URL("../dist/", import.meta.url);
    if (!existsSync(distDirectory)) {
      return;
    }
    expect(existsSync(new URL("404.html", distDirectory))).toBe(false);
  });

  test("detects the splat-to-index rule shape", () => {
    expect(isSplatToIndexRule("/* /index.html 200")).toBe(true);
    expect(isSplatToIndexRule("/app/* /index 200")).toBe(true);
    expect(isSplatToIndexRule("/account /index.html 200")).toBe(false);
    expect(isSplatToIndexRule("/* /app.html 200")).toBe(false);
  });
});
