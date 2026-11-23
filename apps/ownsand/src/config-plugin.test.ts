// SPDX-License-Identifier: Apache-2.0
import type { RsbuildPlugin } from "@rsbuild/core";
import { describe, expect, test } from "@rstest/core";
import { pluginConsoleConfig } from "./config-plugin.ts";

type EnvironmentConfigHook = (config: { mode: string }) => void;

/**
 * Runs the plugin's `setup` with a fake plugin API and returns the callback
 * it registered with `modifyEnvironmentConfig`.
 */
function environmentConfigHook(plugin: RsbuildPlugin): EnvironmentConfigHook {
  let hook: EnvironmentConfigHook | undefined;
  const api = {
    modifyEnvironmentConfig(callback: EnvironmentConfigHook) {
      hook = callback;
    },
  };
  void plugin.setup(api as unknown as Parameters<RsbuildPlugin["setup"]>[0]);
  if (hook === undefined) {
    throw new Error("the plugin did not register modifyEnvironmentConfig");
  }
  return hook;
}

const PRODUCTION_DIAGNOSTIC =
  "PUBLIC_OWNSAND_MOCK_API must not be set for a production build";

describe("console configuration plugin", () => {
  test.each(["true", " true ", "\ttrue\n"])(
    "rejects the mock flag %j when the resolved mode is production",
    (value) => {
      const hook = environmentConfigHook(
        pluginConsoleConfig({ PUBLIC_OWNSAND_MOCK_API: value }),
      );
      expect(() => hook({ mode: "production" })).toThrow(PRODUCTION_DIAGNOSTIC);
    },
  );

  test("allows the mock flag when the resolved mode is development", () => {
    const hook = environmentConfigHook(
      pluginConsoleConfig({ PUBLIC_OWNSAND_MOCK_API: " true " }),
    );
    expect(() => hook({ mode: "development" })).not.toThrow();
  });

  test("the inherited NODE_ENV does not decide the check", () => {
    const original = process.env.NODE_ENV;
    try {
      const hook = environmentConfigHook(
        pluginConsoleConfig({ PUBLIC_OWNSAND_MOCK_API: "true" }),
      );
      process.env.NODE_ENV = "development";
      expect(() => hook({ mode: "production" })).toThrow(PRODUCTION_DIAGNOSTIC);
      process.env.NODE_ENV = "production";
      expect(() => hook({ mode: "development" })).not.toThrow();
    } finally {
      if (original === undefined) {
        delete process.env.NODE_ENV;
      } else {
        process.env.NODE_ENV = original;
      }
    }
  });

  test.each(["false", " false ", "", undefined])(
    "accepts the mock flag %j in a production build",
    (value) => {
      const hook = environmentConfigHook(
        pluginConsoleConfig({ PUBLIC_OWNSAND_MOCK_API: value }),
      );
      expect(() => hook({ mode: "production" })).not.toThrow();
    },
  );

  test.each(["production", "development", "none"])(
    "rejects an invalid mock flag in the %s mode",
    (mode) => {
      const hook = environmentConfigHook(
        pluginConsoleConfig({ PUBLIC_OWNSAND_MOCK_API: "yes" }),
      );
      expect(() => hook({ mode })).toThrow(
        "PUBLIC_OWNSAND_MOCK_API must be true or false",
      );
    },
  );
});
