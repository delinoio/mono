// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

/** Opens the create dialog on the Organizations page and types a name. */
async function openAndName(
  transport: ReturnType<typeof holdingTransport>["transport"],
  name: string,
) {
  renderConsole("/", { transport });
  await screen.findByRole("link", { name: /Acme Robotics/ });
  await act(async () => {
    fireEvent.click(
      screen.getByRole("button", { name: "Create organization" }),
    );
  });
  await settle(60);
  const dialog = screen.getByRole("dialog", { name: "Create an organization" });
  await act(async () => {
    fireEvent.input(
      within(dialog).getByRole("textbox", { name: "Organization name" }),
      { target: { value: name } },
    );
  });
  return dialog;
}

const submit = async (dialog: HTMLElement) => {
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
  });
  await settle(60);
};

describe("create organization navigation", () => {
  test("opens the new organization without waiting for the list refetch", async () => {
    // The page's first load of the list passes; the refetch that follows the
    // create is held.
    let hold = false;
    const { transport, calls } = holdingTransport(
      ["ListOrganizations"],
      () => hold,
    );
    const dialog = await openAndName(transport, "Initech");
    hold = true;
    await submit(dialog);
    expect(calls.ListOrganizations?.length).toBeGreaterThan(0);
    expect(window.location.pathname).toMatch(
      /^\/orgs\/[0-9a-f-]{36}\/sandboxes$/,
    );
    expect(
      screen.queryByRole("dialog", { name: "Create an organization" }),
    ).toBeNull();
    // The switcher shows the new organization once the list RPC settles.
    await act(async () => {
      for (const call of calls.ListOrganizations ?? []) call.settle();
    });
    await settle(100);
    expect(window.location.pathname).toMatch(
      /^\/orgs\/[0-9a-f-]{36}\/sandboxes$/,
    );
    expect(
      screen
        .getByRole("combobox", { name: "Organization" })
        .textContent?.includes("Initech"),
    ).toBe(true);
  });

  test("a failed creation keeps the dialog with its error and a retry works", async () => {
    const { transport, calls, seen } = holdingTransport(["CreateOrganization"]);
    const dialog = await openAndName(transport, "Initech");
    const listRequests = seen.ListOrganizations;
    await submit(dialog);
    await act(async () => {
      calls.CreateOrganization?.[0]?.settle(
        new ConnectError("the request failed", Code.Unavailable),
      );
    });
    await settle(60);
    expect(window.location.pathname).toBe("/");
    expect(dialog.querySelector(".notice-error")).not.toBeNull();
    expect(seen.ListOrganizations).toBe(listRequests);
    expect(
      (
        within(dialog).getByRole("button", {
          name: "Create",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
    await submit(dialog);
    await act(async () => {
      calls.CreateOrganization?.[1]?.settle();
    });
    await settle(100);
    expect(window.location.pathname).toMatch(
      /^\/orgs\/[0-9a-f-]{36}\/sandboxes$/,
    );
  });
});
