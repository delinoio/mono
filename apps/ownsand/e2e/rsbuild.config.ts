// SPDX-License-Identifier: Apache-2.0
import { fileURLToPath } from "node:url";
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

// The browser suite serves RPC fixtures and can select a synthetic continuation failure.
export default mergeRsbuildConfig(consoleConfig, {
  source: {
    entry: {
      index: "./src/index.tsx",
      "sandbox-idempotency": "./e2e/sandbox-idempotency-fixture.ts",
    },
  },
  resolve: {
    alias: {
      "./mock/mock-api.ts$": fileURLToPath(
        new URL("./pagination-mock.ts", import.meta.url),
      ),
    },
  },
});
