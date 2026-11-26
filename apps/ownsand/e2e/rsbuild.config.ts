// SPDX-License-Identifier: Apache-2.0
import { fileURLToPath } from "node:url";
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

// Only the browser suite serves RPC fixtures and can select synthetic list request failures.
export default mergeRsbuildConfig(consoleConfig, {
  // Compile the console and the RPC fixture together before concurrent tests
  // navigate to either entry.
  dev: { lazyCompilation: false },
  source: {
    entry: {
      index: "./src/index.tsx",
      "enrollment-token": "./e2e/enrollment-token-fixture.ts",
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
