// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { MOCK_IDS } from "./mock/mock-api.ts";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("create sandbox navigation", () => {
  test("opens the new sandbox without waiting for the list refetch", async () => {
    // The page's first load of the list passes; the refetch that the
    // invalidation after the create starts is held.
    let hold = false;
    const { transport, calls } = holdingTransport(
      ["ListSandboxes"],
      () => hold,
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Create sandbox" }),
      );
    });
    await settle(60);
    const dialog = await screen.findByRole("dialog", {
      name: "Create a sandbox",
    });
    hold = true;
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
    // The list refetch is still held, so the invalidation did run, and the
    // console already moved on to the new sandbox.
    expect(calls.ListSandboxes).toHaveLength(1);
    expect(window.location.pathname).toMatch(
      new RegExp(`^/orgs/${MOCK_IDS.acme}/sandboxes/[0-9a-f-]{36}$`),
    );
    expect(
      screen.queryByRole("dialog", { name: "Create a sandbox" }),
    ).toBeNull();
    expect(
      await screen.findByRole("heading", { name: "Sandbox" }),
    ).toBeTruthy();
    await act(async () => {
      calls.ListSandboxes?.[0]?.settle();
    });
    await settle(60);
    expect(window.location.pathname).toMatch(/\/sandboxes\/[0-9a-f-]{36}$/);
  });

  test("keeps the dialog open with the error and reuses the idempotency key on retry", async () => {
    const keys: string[] = [];
    let failures = 1;
    const { transport } = holdingTransport(["CreateSandbox"], () => false);
    const failing = {
      ...transport,
      async unary(...args: Parameters<typeof transport.unary>) {
        if (args[0].name === "CreateSandbox") {
          keys.push((args[4] as { idempotencyKey: string }).idempotencyKey);
          if (failures-- > 0) {
            throw new ConnectError("The control plane is busy.", Code.Internal);
          }
        }
        return transport.unary(...args);
      },
    };
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport: failing as typeof transport,
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Create sandbox" }),
      );
    });
    await settle(60);
    const dialog = await screen.findByRole("dialog", {
      name: "Create a sandbox",
    });
    const create = async () => {
      await act(async () => {
        fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
      });
      await settle(60);
    };
    await create();
    expect(dialog.querySelector(".notice-error")).not.toBeNull();
    expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    await create();
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    expect(window.location.pathname).toMatch(/\/sandboxes\/[0-9a-f-]{36}$/);
  });
});
