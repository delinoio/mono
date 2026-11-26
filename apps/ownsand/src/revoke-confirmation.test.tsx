// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { toast } from "delino-ui/toast";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { createMockTransport, MOCK_IDS } from "./mock/mock-api.ts";
import {
  FIRST_REVOCATION_ID,
  INVITATION_PREFIX,
  revokeCollisionTransport,
  SECOND_REVOCATION_ID,
  TOKEN_PREFIX,
} from "./mock/revoke-collision-fixture.ts";
import { renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  // The toaster and its announcers are module state outside the rendered tree.
  toast.dismiss();
  for (const announcer of document.querySelectorAll(
    "[data-ui-sdk-announcer]",
  )) {
    announcer.textContent = "";
  }
});

/** A transport that counts the revoke requests and fails the first `failures`. */
function revokeTransport(method: string, failures = 0) {
  const mock = createMockTransport();
  const state = { calls: 0 };
  const transport: Transport = {
    ...mock,
    async unary(m, ...rest) {
      if (m.name === method) {
        state.calls += 1;
        if (state.calls <= failures) {
          throw new ConnectError("revoke failed", Code.Unavailable);
        }
      }
      return mock.unary(m, ...rest);
    },
  };
  return { transport, state };
}

async function press(element: HTMLElement) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle(60);
}

const noticeIn = (dialog: HTMLElement) => dialog.querySelector(".notice-error");

interface Flow {
  name: string;
  method: string;
  path: string;
  /** The row's visible state after a revoke, and the dialog's names. */
  prefix: string;
  title: string;
  confirm: string;
  success: string;
  /** Prepares the page and returns the transport. */
  prepare: (transport: Transport) => Promise<void>;
  /** The text that identifies the row after the revoke succeeded. */
  revokedText: RegExp;
}

const FLOWS: Flow[] = [
  {
    name: "enrollment token",
    method: "RevokeEnrollmentToken",
    path: `/orgs/${MOCK_IDS.acme}/enrollment-tokens`,
    prefix: "ose_",
    title: "Revoke token",
    confirm: "Revoke token",
    success: "The token is revoked.",
    prepare: async (transport) => {
      await createClient(DeviceService, transport).createEnrollmentToken({
        organizationId: MOCK_IDS.acme,
        ttl: { seconds: 3600n },
      });
    },
    revokedText: /Revoked/,
  },
  {
    name: "invitation",
    method: "RevokeInvitation",
    path: `/orgs/${MOCK_IDS.acme}/members`,
    prefix: "osi_7f3a",
    title: "Revoke invitation",
    confirm: "Revoke invitation",
    success: "The invitation is revoked.",
    prepare: async () => {},
    revokedText: /No pending invitations/,
  },
];

describe.each(FLOWS)("revoking a $name", (flow) => {
  async function start(failures = 0) {
    const { transport, state } = revokeTransport(flow.method, failures);
    await flow.prepare(transport);
    renderConsole(flow.path, { transport });
    await screen.findByRole("heading", { level: 1 });
    await settle(60);
    return state;
  }

  /** The row's Revoke button; a pending row is the only one with it. */
  const revokeButton = () => screen.getByRole("button", { name: /Revoke/ });

  async function open() {
    await press(revokeButton());
    return screen.findByRole("alertdialog", {
      name: new RegExp(`^${flow.title} ${flow.prefix}.*…\\?$`),
    });
  }

  test("asks first, with Cancel focused, and sends no request until confirmed", async () => {
    const state = await start();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const dialog = await open();
    expect(state.calls).toBe(0);
    expect(document.activeElement).toBe(
      within(dialog).getByRole("button", { name: "Cancel" }),
    );
    expect(
      within(dialog).getByRole("button", { name: flow.confirm }),
    ).toBeTruthy();
  });

  test("Cancel and Escape change nothing", async () => {
    const state = await start();
    let dialog = await open();
    await press(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    dialog = await open();
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(state.calls).toBe(0);
    expect(revokeButton()).toBeTruthy();
  });

  test("confirming revokes once and shows the success toast", async () => {
    const state = await start();
    const dialog = await open();
    await press(within(dialog).getByRole("button", { name: flow.confirm }));
    expect(state.calls).toBe(1);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText(flow.revokedText)).toBeTruthy();
    expect(screen.getAllByText(flow.success).length).toBeGreaterThan(0);
  });

  test("a failed revoke keeps the dialog open with the notice and retries on a second confirm", async () => {
    const state = await start(1);
    const dialog = await open();
    await press(within(dialog).getByRole("button", { name: flow.confirm }));
    expect(state.calls).toBe(1);
    const still = screen.getByRole("alertdialog");
    expect(noticeIn(still)?.textContent).toContain("not available right now");
    // The failure shows in the dialog only, not as a toast.
    expect(screen.queryAllByText(/not available right now/)).toHaveLength(1);
    await press(within(still).getByRole("button", { name: flow.confirm }));
    expect(state.calls).toBe(2);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText(flow.revokedText)).toBeTruthy();
  });
});

describe.each([
  {
    path: "enrollment-tokens",
    list: "Enrollment tokens",
    prefix: TOKEN_PREFIX,
    otherPrefix: "ose_34cd",
    action: "Revoke enrollment token",
    confirm: "Revoke token",
    idField: "enrollmentTokenId",
  },
  {
    path: "members",
    list: "Pending invitations",
    prefix: INVITATION_PREFIX,
    otherPrefix: "osi_34cd",
    action: "Revoke Member invitation",
    confirm: "Revoke invitation",
    idField: "invitationId",
  },
])("$list with repeated prefixes", (flow) => {
  const title = (id?: string) =>
    `${flow.confirm} ${flow.prefix}…${id ? ` (${id})` : ""}?`;
  const button = (id?: string) =>
    screen.findByRole("button", {
      name: `${flow.action} ${flow.prefix}${id ? ` (${id})` : ""}`,
    });
  const dialog = (id?: string) =>
    screen.findByRole("alertdialog", { name: title(id) });

  function start(options?: Parameters<typeof revokeCollisionTransport>[1]) {
    const { transport, requests } = revokeCollisionTransport(
      createMockTransport(),
      options,
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/${flow.path}`, { transport });
    return requests;
  }

  function holdPage() {
    let release!: () => void;
    const continuation = new Promise<void>((resolve) => {
      release = resolve;
    });
    return { release, waitForContinuation: () => continuation };
  }

  test("names both rows and confirms only the selected record", async () => {
    const requests = start();
    for (const id of [FIRST_REVOCATION_ID, SECOND_REVOCATION_ID]) {
      const action = await button(id);
      const row = action.closest("li")!;
      expect(within(row).getByText(id)).toBeTruthy();
      expect(within(row).getAllByRole("button")).toEqual([action]);
      expect(action.textContent).toBe("Revoke");
    }

    await press(await button(FIRST_REVOCATION_ID));
    await press(
      within(await dialog(FIRST_REVOCATION_ID)).getByRole("button", {
        name: "Cancel",
      }),
    );
    expect(requests).toEqual([]);

    await press(await button(SECOND_REVOCATION_ID));
    await press(
      within(await dialog(SECOND_REVOCATION_ID)).getByRole("button", {
        name: flow.confirm,
      }),
    );
    expect(requests).toEqual([
      { organizationId: MOCK_IDS.acme, [flow.idField]: SECOND_REVOCATION_ID },
    ]);
    expect(await button()).toBeTruthy();
  });

  test("updates an open confirmation when another page introduces a collision", async () => {
    const { release, waitForContinuation } = holdPage();
    const requests = start({ waitForContinuation });
    await press(await button());
    expect(await dialog()).toBeTruthy();

    await act(async () => release());
    const updated = await dialog(FIRST_REVOCATION_ID);
    expect(within(updated).getByRole("button", { name: "Cancel" })).toBe(
      document.activeElement,
    );
    await press(within(updated).getByRole("button", { name: flow.confirm }));
    expect(requests).toEqual([
      { organizationId: MOCK_IDS.acme, [flow.idField]: FIRST_REVOCATION_ID },
    ]);
    expect(await button()).toBeTruthy();
  });

  test.each(["unique", "duplicate", "inactive"] as const)(
    "keeps the usual copy when the continuation is %s",
    async (variant) => {
      const { release, waitForContinuation } = holdPage();
      start({ variant, waitForContinuation });
      await press(await button());
      expect(await dialog()).toBeTruthy();

      await act(async () => release());
      await waitFor(() =>
        expect(screen.queryByText(/^Loading more /)).toBeNull(),
      );
      await press(
        within(await dialog()).getByRole("button", { name: "Cancel" }),
      );
      expect(await button()).toBeTruthy();
      expect(screen.queryByText(FIRST_REVOCATION_ID)).toBeNull();
      expect(screen.queryByText(SECOND_REVOCATION_ID)).toBeNull();
      if (variant === "unique") {
        await press(
          screen.getByRole("button", {
            name: `${flow.action} ${flow.otherPrefix}`,
          }),
        );
        expect(
          await screen.findByRole("alertdialog", {
            name: `${flow.confirm} ${flow.otherPrefix}…?`,
          }),
        ).toBeTruthy();
      }
    },
  );
});
