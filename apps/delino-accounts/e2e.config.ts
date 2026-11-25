// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";
import type { E2EConfig } from "e2e";
import { web } from "@e2e-dev/web";
import { gateway } from "ai";
import {
  API_ORIGIN,
  LOGTO_APP_ID,
  LOGTO_ENDPOINT,
} from "./e2e/fixture-session.ts";

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
  projectId: "delino-accounts",
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
          // `PUBLIC_LOGTO_ENDPOINT` or a configured Logto registration) cannot
          // break the suite. `rsbuild.config.ts` inlines the `PUBLIC_*` values
          // from `process.env` itself, because the dotenv step no longer does.
          args: [
            rsbuildBin,
            "dev",
            "--no-env",
            "--host",
            "127.0.0.1",
            "--port",
            "{port}",
          ],
          // The runner passes only PATH, HOME and temporary-directory variables
          // to the app command. Pass a nondefault API origin explicitly so
          // `e2e/home.e2e.ts` can observe that the build-time variable reaches
          // the client bundle. Keep it equal to `E2E_API_ORIGIN` in that file.
          env: {
            PUBLIC_API_ORIGIN: API_ORIGIN,
            PUBLIC_LOGTO_ENDPOINT: LOGTO_ENDPOINT,
            PUBLIC_LOGTO_APP_ID: LOGTO_APP_ID,
          },
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
