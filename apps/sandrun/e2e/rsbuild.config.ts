// SPDX-License-Identifier: Apache-2.0
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

export default mergeRsbuildConfig(consoleConfig, {
  source: {
    entry: {
      index: "./src/index.tsx",
      "direct-action-fixture": "./e2e/direct-action-fixture.tsx",
    },
  },
});
