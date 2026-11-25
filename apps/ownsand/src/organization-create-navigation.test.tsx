// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import type { CreateOrganizationResponse } from "@delino/ownsand-protos/v1/organization_pb";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { createMockTransport } from "./mock/mock-api.ts";
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

  test.each([
    { reply: "no organization", id: undefined },
    { reply: "an empty ID", id: "" },
    { reply: "a malformed ID", id: "not-an-organization-id" },
    { reply: "a UUID v4 ID", id: "0193a000-0000-4000-8000-000000000011" },
  ])(
    "keeps the name and route when the response has $reply",
    async ({ id }) => {
      const mock = createMockTransport();
      const incompleteCreate: Transport = {
        ...mock,
        async unary(method, ...args) {
          const response = await mock.unary(method, ...args);
          if (method.name !== "CreateOrganization") return response;
          const { organization } =
            response.message as unknown as CreateOrganizationResponse;
          return {
            ...response,
            message: {
              ...response.message,
              organization:
                id !== undefined && organization
                  ? { ...organization, organizationId: id }
                  : undefined,
            },
          };
        },
      };
      const { transport, seen } = holdingTransport(
        [],
        undefined,
        incompleteCreate,
      );
      const dialog = await openAndName(transport, "Initech");
      await submit(dialog);
      expect(window.location.pathname).toBe("/");
      expect(within(dialog).getByRole("alert").textContent).toContain(
        "The organization may have been created. Close this dialog and check the organization list before trying again.",
      );
      expect(
        (
          within(dialog).getByRole("textbox", {
            name: "Organization name",
          }) as HTMLInputElement
        ).value,
      ).toBe("Initech");
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      await screen.findByRole("link", { name: /Initech/ });
      expect(seen.CreateOrganization).toBe(1);
    },
  );

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
