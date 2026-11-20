// SPDX-License-Identifier: Apache-2.0
import type { E2EConfig } from "e2e";
import { web } from "@e2e-dev/web";
import { gateway } from "ai";
import { E2E_API_ORIGIN } from "./e2e/mock-api.ts";

export default {
  projectId: "ownsand-console",
  tests: "e2e/**/*.e2e.ts",
  targets: [
    {
      engine: web(),
      app: {
        // Port 0 selects a free loopback port, so the suite does not conflict
        // with the fixed development port used by `pnpm dev`.
        url: "http://127.0.0.1:0",
        command: {
          executable: "rsbuild",
          args: ["dev", "--host", "127.0.0.1", "--port", "{port}"],
          // The runner passes only PATH, HOME and temporary-directory
          // variables. The API origin is a loopback origin that nothing
          // listens on: the tests answer every request with `browser.route`.
          // PUBLIC_OWNSAND_E2E_FAKE_AUTH replaces Logto with the development
          // stand-in in src/auth/fake-auth.tsx, so no real Logto is used.
          env: {
            PUBLIC_API_ORIGIN: E2E_API_ORIGIN,
            PUBLIC_OWNSAND_E2E_FAKE_AUTH: "1",
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
