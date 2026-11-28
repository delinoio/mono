// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { describe, test } from "@e2e-dev/web";
import { rspressBin } from "./rspress-bin";

const run = promisify(execFile);
const appDirectory = fileURLToPath(new URL("..", import.meta.url));
const configPath = fileURLToPath(
  new URL("./rspress-artifact.config.ts", import.meta.url),
);
const markdownAlternate =
  /<link\b(?=[^>]*\srel="alternate")(?=[^>]*\stype="text\/markdown")[^>]*>/g;

async function checkMarkdownDiscovery(outDir: string) {
  const [notFoundHtml, homeHtml] = await Promise.all([
    readFile(join(outDir, "404.html"), "utf8"),
    readFile(join(outDir, "index.html"), "utf8"),
  ]);

  assert.ok(
    /<a\b(?=[^>]*\shref="\/")(?=[^>]*\sclass="[^"]*\brp-not-found__home-link\b)[^>]*>/.test(
      notFoundHtml,
    ),
    "404.html must retain its home link",
  );
  assert.deepEqual(
    {
      alternates: notFoundHtml.match(markdownAlternate) ?? [],
      advertises404Markdown: notFoundHtml
        .replace(markdownAlternate, "")
        .includes("/404.md"),
    },
    { alternates: [], advertises404Markdown: false },
    "404.html must omit Markdown discovery for the error page",
  );

  const homeAlternates = homeHtml.match(markdownAlternate) ?? [];
  assert.equal(
    homeAlternates.length,
    1,
    "index.html must have one Markdown alternate",
  );
  assert.match(homeAlternates[0], /\shref="\/index\.md"/);
  const homeHint = homeHtml.match(
    /<div\b[^>]*\sclass="rp-llms-hint"[^>]*>([^<]*)<\/div>/,
  )?.[1];
  assert.ok(homeHint, "index.html must include a Markdown discovery hint");
  for (const file of ["index.md", "llms.txt", "llms-full.txt"]) {
    assert.ok(homeHint.includes(`/${file}`), `home hint must mention /${file}`);
    assert.ok(
      (await readFile(join(outDir, file), "utf8")).trim(),
      `${file} must contain content`,
    );
  }
}

describe("production Markdown discovery", () => {
  test("advertises generated Markdown on the home page and omits it from the 404 page", async () => {
    // Use the production config with a separate output directory so this
    // check can run in parallel with the normal build.
    const outDir = await mkdtemp(join(tmpdir(), "delino-site-artifacts-"));
    try {
      await run(
        process.execPath,
        [rspressBin(), "build", "--config", configPath],
        {
          cwd: appDirectory,
          env: { ...process.env, DELINO_SITE_ARTIFACT_TEST_OUT_DIR: outDir },
          timeout: 60_000,
        },
      );
      await checkMarkdownDiscovery(outDir);
    } finally {
      await rm(outDir, { recursive: true, force: true });
    }
  });
});
