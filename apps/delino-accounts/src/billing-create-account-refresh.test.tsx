// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { createFakeApi, type FakeApi } from "./testing/fake-api.ts";
import { deferNextCall } from "./testing/form-submit-deferral.ts";
import { renderApp, settle } from "./testing/render.tsx";

/**
 * Creating a billing account closes the dialog and opens the new account as
 * soon as the create request succeeds. The refresh of the account list runs
 * in the background and must not keep the dialog open.
 */

async function click(element: Element) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle();
}

async function type(element: Element, value: string) {
  await act(async () => {
    fireEvent.input(element, { target: { value } });
  });
}

function calls(api: FakeApi, method: string) {
  return api.calls.filter((c) => c.method === method).map((c) => c.request);
}

/** Opens the create dialog of the billing list and returns its name field. */
async function openDialog() {
  await screen.findByRole("link", { name: /Acme Robotics/ });
  await click(screen.getByRole("button", { name: "Create billing account" }));
  await settle(60);
  return screen.getByRole("textbox", { name: "Name" }) as HTMLInputElement;
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("billing account creation", () => {
  test("keeps the route selected through Back when an abandoned create succeeds", async () => {
    const api = createFakeApi();
    const { queryClient } = renderApp("/", { api, strict: true });
    await click(screen.getByRole("link", { name: "Manage billing" }));
    const input = await openDialog();

    const listQuery = queryClient.getQueryCache().find({
      queryKey: createConnectQueryKey({
        schema: BillingAccountService.method.listBillingAccounts,
        cardinality: undefined,
      }),
      exact: false,
    });
    expect(listQuery?.state.isInvalidated).toBe(false);

    const held = deferNextCall(api, "CreateBillingAccount");
    try {
      await type(input, "Northwind");
      await click(
        within(screen.getByRole("dialog")).getByRole("button", {
          name: "Create",
        }),
      );
      expect(calls(api, "CreateBillingAccount")).toEqual([
        expect.objectContaining({ name: "Northwind" }),
      ]);

      await act(async () => {
        window.history.replaceState(null, "", "/");
        window.dispatchEvent(new PopStateEvent("popstate"));
      });
      const historyLength = window.history.length;
      expect(
        screen.getByRole("heading", { name: "Delino Accounts" }),
      ).toBeTruthy();
      expect(screen.queryByRole("dialog")).toBeNull();

      held.release();
      await waitFor(() => expect(listQuery?.state.isInvalidated).toBe(true));
      expect(window.location.pathname).toBe("/");
      expect(window.history.length).toBe(historyLength);
      expect(
        screen.getByRole("heading", { name: "Delino Accounts" }),
      ).toBeTruthy();

      await click(screen.getByRole("link", { name: "Manage billing" }));
      expect(
        await screen.findByRole("link", { name: /Northwind/ }),
      ).toBeTruthy();
      expect(calls(api, "CreateBillingAccount")).toHaveLength(1);
    } finally {
      held.release();
    }
  });

  test("opens the new account while the refresh of the list is still pending", async () => {
    const api = createFakeApi();
    renderApp("/billing", { api });
    const input = await openDialog();
    // Hold every later read of the list until the test releases it.
    let release!: () => void;
    api.gates.set(
      "ListBillingAccounts",
      new Promise<void>((resolve) => {
        release = resolve;
      }),
    );
    try {
      await type(input, "Northwind");
      await click(
        within(screen.getByRole("dialog")).getByRole("button", {
          name: "Create",
        }),
      );
      // The account opens although the list refresh did not finish.
      expect(
        await screen.findByRole("heading", { name: "Northwind", level: 1 }),
      ).toBeTruthy();
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(calls(api, "CreateBillingAccount")).toEqual([
        expect.objectContaining({ name: "Northwind" }),
      ]);
    } finally {
      api.gates.delete("ListBillingAccounts");
      release();
    }

    // After the list read succeeds, the list shows the new account.
    await click(screen.getByRole("link", { name: "Billing accounts" }));
    expect(await screen.findByRole("link", { name: /Northwind/ })).toBeTruthy();
    expect(screen.getByRole("link", { name: /Acme Robotics/ })).toBeTruthy();
    expect(calls(api, "CreateBillingAccount")).toHaveLength(1);
  });

  test("a failed create keeps the dialog and the name, and a retry succeeds", async () => {
    const api = createFakeApi();
    renderApp("/billing", { api });
    const input = await openDialog();
    api.failNext.set(
      "CreateBillingAccount",
      new ConnectError("busy", Code.Unavailable),
    );
    await type(input, "Northwind");
    await click(
      within(screen.getByRole("dialog")).getByRole("button", {
        name: "Create",
      }),
    );
    await settle(60);

    // The dialog stays with the entered name, shows an error and can retry.
    const dialog = screen.getByRole("dialog");
    expect(
      (
        within(dialog).getByRole("textbox", {
          name: "Name",
        }) as HTMLInputElement
      ).value,
    ).toBe("Northwind");
    expect(within(dialog).getByRole("alert")).toBeTruthy();
    const create = within(dialog).getByRole("button", {
      name: "Create",
    }) as HTMLButtonElement;
    await waitFor(() => expect(create.disabled).toBe(false));
    expect(window.location.pathname).toBe("/billing");

    await click(create);
    expect(
      await screen.findByRole("heading", { name: "Northwind", level: 1 }),
    ).toBeTruthy();
    expect(calls(api, "CreateBillingAccount")).toHaveLength(2);
  });
});
