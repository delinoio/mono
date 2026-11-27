// SPDX-License-Identifier: Apache-2.0
import { fileURLToPath } from "node:url";
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

export default mergeRsbuildConfig(consoleConfig, {
  resolve: {
    alias: {
      "./mock/mock-api.ts$": fileURLToPath(
        new URL("./credential-mock.ts", import.meta.url),
      ),
    },
  },
});
