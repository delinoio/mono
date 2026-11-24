// SPDX-License-Identifier: Apache-2.0
import { fileURLToPath } from "node:url";
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

// Only the browser suite can select synthetic list request failures.
export default mergeRsbuildConfig(consoleConfig, {
  resolve: {
    alias: {
      "./mock/mock-api.ts$": fileURLToPath(
        new URL("./pagination-mock.ts", import.meta.url),
      ),
    },
  },
});
