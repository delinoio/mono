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
  // Workaround: raise the default per-test timeout from 5 s to 30 s. The
  // billing paging tests render 200-row lists in happy-dom, which is slow
  // under CI load: even after the row-counting query fix, the 5 s default
  // timed out at 5.1 to 7.2 s in CI. Scope: every test in the delino-accounts
  // project; individual tests may still pass a larger explicit timeout (for
  // example HEAVY_LIST_TEST_TIMEOUT_MS in src/billing.test.tsx). Remove it
  // when those paging tests render fewer rows or the tests run reliably under
  // 5 s in CI.
  testTimeout: 30000,
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
