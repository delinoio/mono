// SPDX-License-Identifier: Apache-2.0
import { configure } from "@testing-library/react";

configure({
  // The UI SDK repeats each field error and toast in a hidden live region
  // ([data-ui-sdk-announcer]) 100 ms later. Text queries ignore that region,
  // so that a check of a visible message does not also find the copy when a
  // slow test reaches it after the 100 ms, or the copy that a test left for
  // the next one.
  defaultIgnore: "script, style, [data-ui-sdk-announcer]",
});
