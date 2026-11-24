// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";
import type { E2EConfig } from "e2e";
import { web } from "@e2e-dev/web";
import { gateway } from "ai";

// The runner starts commands without a shell, so it needs the CLI file behind
// pnpm's Windows command shim.
const manifestPath = createRequire(import.meta.url).resolve(
  "@rsbuild/core/package.json",
);
const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
  bin: { rsbuild: string };
};
const rsbuildBin = resolve(dirname(manifestPath), manifest.bin.rsbuild);

export default {
  projectId: "tide-console",
  tests: "e2e/**/*.e2e.ts",
  targets: [
    {
      engine: web(),
      app: {
        // Port 0 selects a free loopback port, so the suite does not conflict
        // with the fixed development port used by `pnpm dev`.
        url: "http://127.0.0.1:0",
        command: {
          executable: process.execPath,
          // `--no-env` stops Rsbuild from loading `.env` and `.env.local` from the
          // app root, so a developer's local values (for example an invalid
          // `PUBLIC_API_ORIGIN`) cannot break the suite.
          args: [
            rsbuildBin,
            "dev",
            "--no-env",
            "--host",
            "127.0.0.1",
            "--port",
            "{port}",
          ],
          // The runner passes only PATH, HOME and temporary-directory
          // variables to the app command. The suite runs against the
          // in-browser fake API with a fake signed-in user, because it has no
          // control plane and no Logto tenant.
          //
          // The value is deliberately padded with spaces. The build must
          // trim it so the mock sign-in and the mock transport agree; if the
          // trimming in rsbuild.config.ts is reverted, sign-in succeeds but
          // the console calls the real API and the seeded data never loads.
          env: { PUBLIC_TIDE_MOCK_API: " true " },
          log: ".e2e/logs/app.log",
        },
      },
    },
  ],
  // Model for `agent.*` steps. `gateway()` reads AI_GATEWAY_API_KEY or a
  // Vercel OIDC token. Tests without agent steps do not call the model.
  agents: {
    default: {
      model: gateway("openai/gpt-6-luna-fast"),
    },
  },
} satisfies E2EConfig;
