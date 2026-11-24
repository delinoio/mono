// SPDX-License-Identifier: Apache-2.0
import { mergeRsbuildConfig } from "@rsbuild/core";
import appConfig from "../rsbuild.config.ts";

export default mergeRsbuildConfig(appConfig, {
  source: {
    entry: {
      index: "./src/index.tsx",
      "billing-settings": "./e2e/billing-settings.fixture.tsx",
    },
  },
});
