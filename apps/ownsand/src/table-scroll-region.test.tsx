// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { cleanup, screen } from "@testing-library/react";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import {
  TEST_SETUP_MS,
  holdingTransport,
  renderConsole,
} from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

afterEach(() => {
  cleanup();
});

const SANDBOXES = `/orgs/${MOCK_IDS.acme}/sandboxes`;
const DEVICES = `/orgs/${MOCK_IDS.acme}/devices`;

/** Every scroll region on the page, read from the DOM. */
const regions = () => document.querySelectorAll(".table-wrap");

describe("the sandbox and device tables", () => {
  test.each([
    ["Sandbox list", SANDBOXES],
    ["Device list", DEVICES],
  ])(
    "%s is a named, keyboard-focusable scroll region",
    async (name, path) => {
      renderConsole(path);
      const region = await screen.findByRole("region", { name }, WAIT);
      expect(region.getAttribute("tabindex")).toBe("0");
      expect(region.classList.contains("table-wrap")).toBe(true);
      // The table stays the only child, so the columns and markup are unchanged.
      expect(region.children).toHaveLength(1);
      expect(region.firstElementChild?.tagName).toBe("TABLE");
      // The region takes keyboard focus.
      region.focus();
      expect(document.activeElement).toBe(region);
    },
    WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "the loading state renders no region",
    async () => {
      const held = holdingTransport(["ListSandboxes"]);
      renderConsole(SANDBOXES, { transport: held.transport });
      expect(
        await screen.findByRole("status", { name: "Loading sandboxes" }, WAIT),
      ).toBeTruthy();
      expect(regions()).toHaveLength(0);
      // Release the held request so no pending work outlives the test.
      for (const call of held.calls.ListSandboxes ?? []) call.settle();
    },
    WAIT.timeout + TEST_SETUP_MS,
  );

  test.each([
    ["No devices yet", "devices"],
    ["No sandboxes match", "sandboxes"],
  ])(
    "the empty state %s renders no region",
    async (title, page) => {
      // Northwind has no devices and no sandboxes.
      renderConsole(`/orgs/${MOCK_IDS.northwind}/${page}`);
      expect(await screen.findByText(title, undefined, WAIT)).toBeTruthy();
      expect(regions()).toHaveLength(0);
    },
    WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "the error state renders no region",
    async () => {
      const mock = createMockTransport();
      const failing: Transport = {
        ...mock,
        async unary(method, ...rest) {
          if (method.name === "ListDevices") {
            throw new ConnectError("devices are unavailable", Code.Unavailable);
          }
          return mock.unary(method, ...rest);
        },
      };
      renderConsole(DEVICES, { transport: failing });
      expect(
        await screen.findByRole("button", { name: "Try again" }, WAIT),
      ).toBeTruthy();
      expect(regions()).toHaveLength(0);
    },
    WAIT.timeout + TEST_SETUP_MS,
  );
});

describe("the billing usage table", () => {
  test(
    "stays in a plain wrapper that is not a focus stop",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
      const table = await screen.findByRole(
        "table",
        { name: "Usage this month" },
        WAIT,
      );
      const wrapper = table.closest(".table-wrap");
      expect(wrapper).not.toBeNull();
      expect(wrapper?.getAttribute("role")).toBeNull();
      expect(wrapper?.getAttribute("aria-label")).toBeNull();
      expect(wrapper?.getAttribute("tabindex")).toBeNull();
    },
    WAIT.timeout + TEST_SETUP_MS,
  );
});
