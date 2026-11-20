// SPDX-License-Identifier: Apache-2.0
import { define } from "rstack";

// Package-local lint rules of the console, run by `pnpm --filter
// ownsand-console lint` with `rstack lint -c lint.config.ts`. The file is not
// named `rstack.config.ts`, so it does not change the repository-wide
// formatter configuration in the root `rstack.config.ts`.
define.lint(({ js, ts }) => [
  { ignores: ["dist/**", ".e2e/**", "node_modules/**"] },
  js.configs.recommended,
  ts.configs.recommendedTypeChecked,
  {
    languageOptions: {
      parserOptions: {
        project: ["./tsconfig.json"],
      },
    },
  },
  {
    // Testing Library flushes React updates with `act(async () => {...})`,
    // which needs an async callback even when it awaits nothing.
    files: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    rules: { "@typescript-eslint/require-await": "off" },
  },
]);
