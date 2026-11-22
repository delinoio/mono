// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rstest/core";

// Workaround: pin the unit-test process to UTC. The fixtures use UTC-midnight
// timestamps and `formatDate` in src/format.ts renders them with the host
// time zone, so without the pin a test passes only when the host zone is at or
// east of UTC (for example it fails on America/Los_Angeles by showing the
// previous day). Scope: the Rstest process and its workers only; the app
// bundle, the e2e suite and the browser time zone are unchanged. Remove the
// pin when `formatDate` renders in an explicit display time zone and the
// fixtures no longer depend on the host zone; then delete the guard test
// "runs in UTC" in src/logic.test.ts.
process.env.TZ = "UTC";

export default defineConfig({
  // Pages need a DOM for routing, dialogs and focus.
  testEnvironment: "happy-dom",
  include: ["src/**/*.test.{ts,tsx}"],
  tools: {
    swc: {
      jsc: {
        transform: {
          react: { runtime: "automatic" },
        },
      },
    },
  },
});
