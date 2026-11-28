// SPDX-License-Identifier: Apache-2.0
import { configure } from "@testing-library/react";

configure({
  // The UI SDK repeats each field error and toast in a hidden live region
  // ([data-ui-sdk-announcer]) 100 ms later. Text queries ignore that region,
  // so that a check of a visible message does not also find the copy when a
  // slow test reaches it after the 100 ms, or the copy that a test left for
  // the next one.
  defaultIgnore: "script, style, [data-ui-sdk-announcer]",
  // Workaround: wait up to 5 s instead of 1 s in findBy* and waitFor. Many
  // tests render 200-row lists in happy-dom, and on a busy CI runner the
  // first rows or a notice after them appeared after more than 1 s. Scope:
  // every test file of delino-accounts; a passing wait still ends as soon as
  // its condition is true, and testTimeout in rstest.config.ts stays the
  // limit of a whole test. A wait that is expected to fail takes the full
  // 5 s ("a failed setup restores the browser stubs and propagates the
  // failure" in src/billing.test.tsx). Remove it when the list tests render
  // fewer rows or finish their waits within 1 s in CI.
  asyncUtilTimeout: 5_000,
});
