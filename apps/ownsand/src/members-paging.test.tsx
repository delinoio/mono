// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
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
  TEST_SETUP_MS,
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
 * The adapter slices the seeded roster and handles its own continuation tokens.
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
    async unary(method, signal, timeout, header, input, contextValues) {
      if (method.name !== "ListMembers") {
        return mock.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
      }
      const token = (input as { pageToken: string }).pageToken;
      state.tokens.push(token);
      const page = token === "" ? 1 : Number(token.slice("page-".length));
      if (state.failPage === page) {
        throw new ConnectError("the page failed", Code.Unavailable);
      }
      // Only the envelope of the response matters; the rows come from `real`.
      const firstPage = { ...input, pageToken: "" };
      const res = await mock.unary(
        method,
        signal,
        timeout,
        header,
        firstPage,
        contextValues,
      );
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
  test(
    "lists the members of every page",
    async () => {
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
    },
    WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "sends one request when the response has no token",
    async () => {
      const { transport, state } = await pagedMembers([[OWNER, ADMIN, MEMBER]]);
      renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
      await waitFor(() => expect(memberRows()).toHaveLength(3), WAIT);
      await flush();
      expect(state.tokens).toEqual([""]);
      expect(screen.queryByText("Loading more members")).toBeNull();
    },
    WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "keeps the loaded members and continues from the token on Try again",
    async () => {
      const { transport, state } = await pagedMembers([
        [OWNER, ADMIN],
        [MEMBER],
      ]);
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
    },
    2 * WAIT.timeout + TEST_SETUP_MS,
  );
});

describe("ownership transfer picker", () => {
  test(
    "offers the non-owner members of every page and confirms the one chosen",
    async () => {
      const { transport, state } = await pagedMembers([
        [OWNER],
        [ADMIN],
        [MEMBER],
      ]);
      const refresh = fullListRefresh(
        MemberService.method.listMembers,
        transport,
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, refresh);
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

      await refresh.refetch();
      expect(state.tokens).toEqual([
        "",
        "page-2",
        "page-3",
        "",
        "page-2",
        "page-3",
      ]);
      expect(
        screen.getByRole("alertdialog", { name: `Make ${MEMBER} the owner?` }),
      ).toBe(dialog);
    },
    3 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "requires a new selection and confirmation after a successful refresh removes the target",
    async () => {
      const pages = [[OWNER, ADMIN, MEMBER]];
      const { transport } = await pagedMembers(pages);
      const held = holdingTransport(
        ["TransferOwnership"],
        () => true,
        transport,
      );
      const refresh = fullListRefresh(
        MemberService.method.listMembers,
        held.transport,
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, refresh);

      await openOwnerPicker();
      await act(async () =>
        fireEvent.click(
          await screen.findByRole("option", { name: "Jun Park" }, WAIT),
        ),
      );
      const picker = screen.getByRole("combobox", { name: "New owner" });
      const transfer = screen.getByRole("button", { name: "Transfer" });
      await act(async () => fireEvent.click(transfer));
      await screen.findByRole(
        "alertdialog",
        { name: "Make Jun Park the owner?" },
        WAIT,
      );

      pages[0] = [OWNER, MEMBER];
      await refresh.refetch();
      await waitFor(
        () => expect(screen.queryByRole("alertdialog")).toBeNull(),
        WAIT,
      );
      expect(picker.textContent).toContain("Choose a member");
      expect(transfer.hasAttribute("disabled")).toBe(true);
      expect(held.seen.TransferOwnership).toBeUndefined();

      pages[0] = [OWNER, ADMIN, MEMBER];
      await refresh.refetch();
      expect(picker.textContent).toContain("Choose a member");
      expect(transfer.hasAttribute("disabled")).toBe(true);
      expect(screen.queryByRole("alertdialog")).toBeNull();

      await openOwnerPicker();
      expect(
        await screen.findByRole("option", { name: "Jun Park" }, WAIT),
      ).toBeTruthy();
      await act(async () =>
        fireEvent.click(screen.getByRole("option", { name: MEMBER })),
      );
      await act(async () => fireEvent.click(transfer));
      const dialog = await screen.findByRole(
        "alertdialog",
        {
          name: `Make ${MEMBER} the owner?`,
        },
        WAIT,
      );
      await act(async () =>
        fireEvent.click(
          within(dialog).getByRole("button", { name: "Transfer ownership" }),
        ),
      );
      expect(held.seen.TransferOwnership).toBe(1);
      expect(held.calls.TransferOwnership?.[0]?.input).toMatchObject({
        organizationId: MOCK_IDS.acme,
        userId: MEMBER,
      });
      await act(async () => held.calls.TransferOwnership![0]!.settle());
    },
    7 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "keeps the pending choice while an incomplete refresh looks for the target on a later page",
    async () => {
      const pages = [[OWNER, ADMIN, MEMBER]];
      const { transport } = await pagedMembers(pages);
      const held = holdingTransport(
        ["ListMembers"],
        () => pages.length > 1,
        transport,
      );
      const refresh = fullListRefresh(
        MemberService.method.listMembers,
        held.transport,
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, refresh);
      await openOwnerPicker();
      await act(async () =>
        fireEvent.click(
          await screen.findByRole("option", { name: "Jun Park" }, WAIT),
        ),
      );
      const transfer = screen.getByRole("button", { name: "Transfer" });
      await act(async () => fireEvent.click(transfer));
      await screen.findByRole(
        "alertdialog",
        { name: "Make Jun Park the owner?" },
        WAIT,
      );

      pages.splice(0, 1, [OWNER, MEMBER], [ADMIN]);
      const refetch = refresh.queryClient.refetchQueries({
        queryKey: createConnectQueryKey({
          schema: MemberService.method.listMembers,
          cardinality: "infinite",
        }),
      });
      await waitFor(() => expect(held.calls.ListMembers).toHaveLength(1), WAIT);
      expect(
        screen.getByRole("alertdialog", { name: "Make Jun Park the owner?" }),
      ).toBeTruthy();
      await act(async () => {
        held.calls.ListMembers![0]!.settle();
        await refetch;
      });
      await waitFor(() => expect(held.calls.ListMembers).toHaveLength(2), WAIT);
      expect(screen.queryByRole("alertdialog")).toBeNull();
      expect(transfer.hasAttribute("disabled")).toBe(true);
      expect(held.seen.TransferOwnership).toBeUndefined();

      await act(async () => held.calls.ListMembers![1]!.settle());
      await screen.findByRole(
        "alertdialog",
        { name: "Make Jun Park the owner?" },
        WAIT,
      );
    },
    7 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "sends one request when the response has no token",
    async () => {
      const { transport, state } = await pagedMembers([[OWNER, ADMIN, MEMBER]]);
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
      await openOwnerPicker();
      await screen.findByRole("option", { name: "Jun Park" }, WAIT);
      await flush();
      expect(state.tokens).toEqual([""]);
    },
    2 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "keeps the owner choice on a transient refresh and clears it on a terminal refresh",
    async () => {
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
        screen
          .getByRole("button", { name: "Transfer" })
          .hasAttribute("disabled"),
      ).toBe(true);
      expect(screen.queryByRole("alertdialog")).toBeNull();
      await openOwnerPicker();
      expect(
        await screen.findByRole("option", { name: "Jun Park" }, WAIT),
      ).toBeTruthy();
      expect(seen.TransferOwnership).toBeUndefined();
    },
    7 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "closes the transfer confirmation and hides cached choices after a terminal continuation error",
    async () => {
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
      expect(
        screen.getByRole("button", { name: "Invite someone" }),
      ).toBeTruthy();
      expect(calls.ListMembers).toHaveLength(2);
    },
    6 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "says that members are missing when a later page fails and recovers on Try again",
    async () => {
      const { transport, state } = await pagedMembers([
        [OWNER, ADMIN],
        [MEMBER],
      ]);
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
    },
    5 * WAIT.timeout + TEST_SETUP_MS,
  );
});
