// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import {
  ListMembersResponseSchema,
  MemberService,
} from "@delino/ownsand-protos/v1/member_pb";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { MOCK_IDS, MOCK_USER, createMockTransport } from "./mock/mock-api.ts";
import {
  fullListRefresh,
  holdingTransport,
  renderConsole,
} from "./test-support.tsx";

const OWNER = MOCK_USER;
const ADMIN = "user-admin-jun";
const MEMBER = "user-member-ada";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  // The toaster is module state outside the rendered tree.
  toast.dismiss();
});

/**
 * A control plane whose ListMembers of the Acme organization returns the
 * seeded members one page each, with the token `page-<n>` that leads to page
 * n. `pages[i]` lists the user IDs of page i + 1; the last page has no token.
 * While `state.failPage` is set, that 1-based page fails with `unavailable`.
 * The mock ignores `page_size`, so the pages are cut here and no test needs
 * more than 200 members.
 */
async function pagedMembers(pages: string[][]) {
  const mock = createMockTransport();
  const real = await createClient(MemberService, mock).listMembers({
    organizationId: MOCK_IDS.acme,
  });
  const state = {
    tokens: [] as string[],
    failPage: undefined as number | undefined,
  };
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      if (method.name !== "ListMembers") return mock.unary(method, ...rest);
      const token = (rest[3] as { pageToken: string }).pageToken;
      state.tokens.push(token);
      const page = token === "" ? 1 : Number(token.slice("page-".length));
      if (state.failPage === page) {
        throw new ConnectError("the page failed", Code.Unavailable);
      }
      // Only the envelope of the response matters; the rows come from `real`.
      const res = await mock.unary(method, ...rest);
      return {
        ...res,
        message: create(ListMembersResponseSchema, {
          members: real.members.filter((m) =>
            pages[page - 1]!.includes(m.userId),
          ),
          nextPageToken: page < pages.length ? `page-${page + 1}` : "",
        }),
      } as unknown as typeof res;
    },
  };
  return { transport, state };
}

/** Lets the requests that a render has queued reach the transport. */
async function flush() {
  await act(async () => {
    await Promise.resolve();
  });
  await act(async () => {
    await Promise.resolve();
  });
}

const memberRows = () =>
  within(screen.getByRole("list", { name: "Members" })).getAllByRole(
    "listitem",
  );

/** Opens the New owner picker of the settings page. */
async function openOwnerPicker() {
  const picker = await screen.findByRole(
    "combobox",
    { name: "New owner" },
    WAIT,
  );
  await act(async () => {
    fireEvent.click(picker);
  });
}

describe("members page", () => {
  test("lists the members of every page", async () => {
    const { transport, state } = await pagedMembers([
      [OWNER],
      [ADMIN],
      [MEMBER],
    ]);
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
    await waitFor(() => expect(memberRows()).toHaveLength(3), WAIT);
    expect(screen.getByText("Mina Kim")).toBeTruthy();
    expect(screen.getByText("Jun Park")).toBeTruthy();
    expect(screen.getByText(MEMBER)).toBeTruthy();
    // The first request has no token; each next one sends the token of the
    // response before it, and the page sends nothing after the last page.
    expect(state.tokens).toEqual(["", "page-2", "page-3"]);
    expect(screen.queryByText("Loading more members")).toBeNull();
    // A member of a later page can be managed like a member of the first.
    expect(
      screen.getByRole("combobox", { name: "Role of Jun Park" }),
    ).toBeTruthy();
  });

  test("sends one request when the response has no token", async () => {
    const { transport, state } = await pagedMembers([[OWNER, ADMIN, MEMBER]]);
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
    await waitFor(() => expect(memberRows()).toHaveLength(3), WAIT);
    await flush();
    expect(state.tokens).toEqual([""]);
    expect(screen.queryByText("Loading more members")).toBeNull();
  });

  test("keeps the loaded members and continues from the token on Try again", async () => {
    const { transport, state } = await pagedMembers([[OWNER, ADMIN], [MEMBER]]);
    state.failPage = 2;
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
    expect(
      await screen.findByText("Some members are not shown yet.", {}, WAIT),
    ).toBeTruthy();
    expect(memberRows()).toHaveLength(2);
    expect(state.tokens).toEqual(["", "page-2"]);

    state.failPage = undefined;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    await waitFor(() => expect(memberRows()).toHaveLength(3), WAIT);
    expect(screen.queryByText("Some members are not shown yet.")).toBeNull();
    // The retry asks for the failed page again; it does not start over.
    expect(state.tokens).toEqual(["", "page-2", "page-2"]);
  });
});

describe("ownership transfer picker", () => {
  test("offers the non-owner members of every page and confirms the one chosen", async () => {
    const { transport, state } = await pagedMembers([
      [OWNER],
      [ADMIN],
      [MEMBER],
    ]);
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
    await openOwnerPicker();
    await screen.findByRole("option", { name: MEMBER }, WAIT);
    expect(screen.getByRole("option", { name: "Jun Park" })).toBeTruthy();
    // The owner is not eligible, whatever page they are on.
    expect(screen.queryByRole("option", { name: "Mina Kim" })).toBeNull();
    expect(state.tokens).toEqual(["", "page-2", "page-3"]);

    await act(async () => {
      fireEvent.click(screen.getByRole("option", { name: MEMBER }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Transfer" }));
    });
    const dialog = await screen.findByRole("alertdialog", {}, WAIT);
    expect(dialog.textContent).toContain(`Make ${MEMBER} the owner?`);
  });

  test("sends one request when the response has no token", async () => {
    const { transport, state } = await pagedMembers([[OWNER, ADMIN, MEMBER]]);
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
    await openOwnerPicker();
    await screen.findByRole("option", { name: "Jun Park" }, WAIT);
    await flush();
    expect(state.tokens).toEqual([""]);
  });

  test("keeps the owner choice on a transient refresh and clears it on a terminal refresh", async () => {
    const { transport, queryClient, refetch, failedTokens, seen } =
      fullListRefresh(MemberService.method.listMembers);

    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      transport,
      queryClient,
    });
    await openOwnerPicker();
    const candidate = await screen.findByRole(
      "option",
      { name: "Jun Park" },
      WAIT,
    );
    await act(async () => {
      fireEvent.click(candidate);
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Transfer" }));
    });
    const dialog = await screen.findByRole("alertdialog", {}, WAIT);

    await refetch(Code.Unavailable);
    expect(failedTokens).toEqual(["", "", ""]);
    expect(dialog.textContent).toContain("Make Jun Park the owner?");
    expect(
      within(dialog)
        .getByRole("button", { name: "Transfer ownership" })
        .hasAttribute("disabled"),
    ).toBe(false);

    await refetch(Code.PermissionDenied);
    expect(
      await screen.findByText(
        "You do not have permission to do this.",
        {},
        WAIT,
      ),
    ).toBeTruthy();
    expect(failedTokens).toEqual(["", "", "", ""]);
    expect(screen.queryByRole("combobox", { name: "New owner" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Transfer" })).toBeNull();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(
      screen.getByRole("button", { name: "Delete organization" }),
    ).toBeTruthy();

    await refetch();
    const picker = await screen.findByRole(
      "combobox",
      { name: "New owner" },
      WAIT,
    );
    expect(picker.textContent).toContain("Choose a member");
    expect(
      screen.getByRole("button", { name: "Transfer" }).hasAttribute("disabled"),
    ).toBe(true);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await openOwnerPicker();
    expect(
      await screen.findByRole("option", { name: "Jun Park" }, WAIT),
    ).toBeTruthy();
    expect(seen.TransferOwnership).toBeUndefined();
  });

  test("closes the transfer confirmation and hides cached choices after a terminal continuation error", async () => {
    const paged = await pagedMembers([[OWNER, ADMIN], [MEMBER]]);
    const { transport, calls, seen } = holdingTransport(
      ["ListMembers"],
      () => true,
      paged.transport,
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
    await waitFor(() => expect(calls.ListMembers).toHaveLength(1), WAIT);
    await act(async () => calls.ListMembers![0]!.settle());
    await openOwnerPicker();
    const candidate = await screen.findByRole(
      "option",
      { name: "Jun Park" },
      WAIT,
    );
    await act(async () => fireEvent.click(candidate));
    await act(async () =>
      fireEvent.click(screen.getByRole("button", { name: "Transfer" })),
    );
    await screen.findByRole(
      "alertdialog",
      { name: "Make Jun Park the owner?" },
      WAIT,
    );

    await act(async () =>
      calls.ListMembers![1]!.settle(
        new ConnectError("the page failed", Code.PermissionDenied),
      ),
    );
    expect(
      await screen.findByText(
        "You do not have permission to do this.",
        {},
        WAIT,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.queryByRole("combobox", { name: "New owner" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Transfer" })).toBeNull();
    expect(screen.queryByText("Some members are not shown yet.")).toBeNull();
    expect(seen.TransferOwnership).toBeUndefined();
    expect(
      screen.getByRole("button", { name: "Delete organization" }),
    ).toBeTruthy();

    await act(async () =>
      fireEvent.click(screen.getByRole("link", { name: "Members" })),
    );
    expect(
      await screen.findByText(
        "You do not have permission to do this.",
        {},
        WAIT,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Members" })).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Remove Jun Park" }),
    ).toBeNull();
    expect(screen.queryByText("Some members are not shown yet.")).toBeNull();
    expect(screen.getByRole("button", { name: "Invite someone" })).toBeTruthy();
    expect(calls.ListMembers).toHaveLength(2);
  });

  test("says that members are missing when a later page fails and recovers on Try again", async () => {
    const { transport, state } = await pagedMembers([[OWNER, ADMIN], [MEMBER]]);
    state.failPage = 2;
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
    expect(
      await screen.findByText("Some members are not shown yet.", {}, WAIT),
    ).toBeTruthy();
    await act(async () => {
      fireEvent.click(screen.getByRole("combobox", { name: "New owner" }));
    });
    await screen.findByRole("option", { name: "Jun Park" }, WAIT);
    expect(screen.queryByRole("option", { name: MEMBER })).toBeNull();
    await act(async () => {
      fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
    });

    state.failPage = undefined;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    await waitFor(
      () =>
        expect(
          screen.queryByText("Some members are not shown yet."),
        ).toBeNull(),
      WAIT,
    );
    await openOwnerPicker();
    await screen.findByRole("option", { name: MEMBER }, WAIT);
    expect(state.tokens).toEqual(["", "page-2", "page-2"]);
  });
});
