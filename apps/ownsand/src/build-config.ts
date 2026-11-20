// SPDX-License-Identifier: Apache-2.0
import { readConsoleConfig, type ConsoleConfig } from "./config.ts";

/**
 * Build-time configuration inlined into the client bundle. This module reads
 * `import.meta.env`, which only the bundler provides, so unit tests import
 * `config.ts` instead.
 */
export const CONSOLE_CONFIG: ConsoleConfig = readConsoleConfig({
  PUBLIC_API_ORIGIN: import.meta.env.PUBLIC_API_ORIGIN,
  PUBLIC_LOGTO_ENDPOINT: import.meta.env.PUBLIC_LOGTO_ENDPOINT,
  PUBLIC_LOGTO_APP_ID: import.meta.env.PUBLIC_LOGTO_APP_ID,
  PUBLIC_OWNSAND_E2E_FAKE_AUTH: import.meta.env.PUBLIC_OWNSAND_E2E_FAKE_AUTH,
});
