// SPDX-License-Identifier: Apache-2.0
import { fileURLToPath } from "node:url";
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

// Only the browser suite serves fixtures and can select synthetic request failures.
export default mergeRsbuildConfig(consoleConfig, {
  // Compile the console and the RPC fixture together before concurrent tests
  // navigate to either entry.
  dev: { lazyCompilation: false },
  source: {
    entry: {
      index: "./src/index.tsx",
      "member-row-identity": "./e2e/member-row-identity-fixture.tsx",
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
