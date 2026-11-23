// SPDX-License-Identifier: Apache-2.0
import type { E2EConfig } from "e2e";
import { web } from "@e2e-dev/web";
import { gateway } from "ai";
import { rspressBin } from "./e2e/rspress-bin";

export default {
  projectId: "delino-docs",
  tests: "e2e/**/*.e2e.ts",
  targets: [
    {
      engine: web(),
      app: {
        // Port 0 selects a free loopback port, so the suite does not conflict
        // with the fixed development port used by `pnpm dev`.
        url: "http://127.0.0.1:0",
        command: {
          // The Rspress CLI file runs on the Node executable, because the
          // runner starts the command without a shell and cannot start the
          // `rspress` command shim of Windows.
          executable: process.execPath,
          args: [
            rspressBin(),
            "dev",
            "--host",
            "127.0.0.1",
            "--port",
            "{port}",
          ],
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
