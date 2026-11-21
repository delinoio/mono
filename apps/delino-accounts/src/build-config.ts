// SPDX-License-Identifier: Apache-2.0
import { type AccountsConfig, readAccountsConfig } from "./config.ts";

/**
 * The configuration inlined into the client bundle at build time. Only the
 * bundler evaluates this module, because it reads `import.meta.env`; tests
 * and `rsbuild.config.ts` import the pure resolver in `config.ts`.
 */
export const ACCOUNTS_CONFIG: AccountsConfig = readAccountsConfig(
  import.meta.env,
);
