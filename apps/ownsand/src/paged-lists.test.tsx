// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { SnapshotState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type ListSnapshotsResponse,
  SnapshotService,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { invalidateServices } from "./api.ts";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

/**
 * The longest wait for one condition. The console renders a list of more than
 * 200 rows several times in these tests, which takes seconds on a loaded CI
 * runner, so a wait that ends on its own condition needs far more than the
 * one second that Testing Library allows by default.
 */
const WAIT_MS = 20_000;
const WAIT = { timeout: WAIT_MS };

/** The timeout of a test that renders a list of more than one page. */
const LONG_LIST_TEST_TIMEOUT_MS = 60_000;

/**
 * Rows that a test adds to the Acme organization after the seeded one. The
 * seeded row and these fill the first page of 200 rows and put one row on the
 * second page, the least that exercises a continuation token.
 */
const EXTRA_ROWS = 200;
const ALL_ROWS = EXTRA_ROWS + 1;
const FIRST_PAGE_ROWS = 200;

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
 * Records the `page_token` of every request of the method. While
 * `state.failLaterPages` is set, every request that has a token (every page
 * after the first one) fails with `unavailable`.
 */
function pagedTransport(method: string, mock: Transport) {
  const state = { tokens: [] as string[], failLaterPages: false };
  const transport: Transport = {
    ...mock,
    async unary(m, ...rest) {
      if (m.name === method) {
        const token = (rest[3] as { pageToken: string }).pageToken;
        state.tokens.push(token);
        if (state.failLaterPages && token !== "") {
          throw new ConnectError("the page failed", Code.Unavailable);
        }
      }
      return mock.unary(m, ...rest);
    },
  };
  return { transport, state };
}

const itemsOf = (name: string) =>
  within(screen.getByRole("list", { name })).getAllByRole("listitem");

/** Waits until the list holds exactly `count` rows, then returns them. */
async function waitForRows(name: string, count: number) {
  await waitFor(() => expect(itemsOf(name)).toHaveLength(count), WAIT);
  return itemsOf(name);
}

describe("snapshot source lists", () => {
  // The mock returns a token after every full page, including the last full
  // page. Use one row per response here to exercise that rule with two rows.
  function sourcePages() {
    const mock = createMockTransport({ extraSnapshots: 1 });
    const state = {
      laterSnapshotError: undefined as Code | undefined,
      laterSnapshotState: SnapshotState.READY,
      tokens: [] as string[],
    };
    const transport: Transport = {
      ...mock,
      async unary(method, signal, timeout, header, input, contextValues) {
        if (method.name !== "ListSnapshots") {
          return mock.unary(
            method,
            signal,
            timeout,
            header,
            input,
            contextValues,
          );
        }
        const request = input as typeof input & { pageToken: string };
        state.tokens.push(request.pageToken);
        if (request.pageToken && state.laterSnapshotError !== undefined) {
          throw new ConnectError(
            "Could not load snapshots",
            state.laterSnapshotError,
          );
        }
        const response = await mock.unary(
          method,
          signal,
          timeout,
          header,
          { ...request, pageSize: 1 },
          contextValues,
        );
        const message = response.message as unknown as ListSnapshotsResponse;
        if (!request.pageToken || message.snapshots.length === 0) {
          return response;
        }
        const duplicate = {
          ...message.snapshots[0]!,
          state: state.laterSnapshotState,
        };
        return {
          ...response,
          message: { ...response.message, snapshots: [duplicate, duplicate] },
        };
      },
    };
    return { transport, state };
  }

  const showMore = () =>
    screen.getByRole("button", { name: "Show more snapshots" });

  test("retries the same token, shows an ID once and stops after an empty last page", async () => {
    const { transport, state } = sourcePages();
    state.laterSnapshotError = Code.Unavailable;
    renderConsole(`/orgs/${MOCK_IDS.acme}/snapshots`, { transport });
    await waitForRows("Snapshots", 1);
    fireEvent.click(showMore());

    const retry = await screen.findByRole("button", { name: "Try again" });
    expect(itemsOf("Snapshots")).toHaveLength(1);
    state.laterSnapshotError = undefined;
    fireEvent.click(retry);
    await waitForRows("Snapshots", 2);
    expect(state.tokens).toEqual(["", state.tokens[1], state.tokens[1]]);
    expect(state.tokens[1]).not.toBe("");

    fireEvent.click(showMore());
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: "Show more snapshots" }),
      ).toBeNull(),
    );
    expect(itemsOf("Snapshots")).toHaveLength(2);
    expect(state.tokens[3]).not.toBe(state.tokens[1]);
  });

  test.each([
    [Code.NotFound, "It was not found, or you no longer have access to it."],
    [Code.PermissionDenied, "You do not have permission to do this."],
    [Code.Unauthenticated, "Your session ended. Sign in again."],
  ] as const)(
    "removes management rows and their continuation after terminal status %s",
    async (code, notice) => {
      const { transport, state } = sourcePages();
      state.laterSnapshotError = code;
      renderConsole(`/orgs/${MOCK_IDS.acme}/snapshots`, { transport });
      await waitForRows("Snapshots", 1);
      await waitForRows("Templates", 1);
      fireEvent.click(showMore());

      await waitFor(() => {
        expect(screen.queryByRole("list", { name: "Snapshots" })).toBeNull();
        expect(
          screen.queryByRole("button", { name: "Show more snapshots" }),
        ).toBeNull();
      });
      expect(screen.getByText(notice)).toBeTruthy();
      expect(itemsOf("Templates")).toHaveLength(1);
    },
  );

  test("offers a later snapshot once it becomes ready after invalidation", async () => {
    const { transport, state } = sourcePages();
    state.laterSnapshotState = SnapshotState.CREATING;
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport,
      queryClient,
    });
    fireEvent.click(
      await screen.findByRole("button", { name: "Create sandbox" }),
    );
    await waitFor(() => expect(state.tokens).toHaveLength(3));
    const source = await screen.findByRole("combobox", { name: "Source" });
    fireEvent.click(source);
    expect(
      await screen.findAllByRole("option", { name: /^Snapshot / }),
    ).toHaveLength(1);

    state.laterSnapshotState = SnapshotState.READY;
    await act(async () => {
      await invalidateServices(queryClient, SnapshotService);
    });
    await waitFor(() =>
      expect(
        screen.getAllByRole("option", { name: /^Snapshot / }),
      ).toHaveLength(2),
    );
    expect(state.tokens).toHaveLength(6);
  });
});

describe("enrollment tokens list", () => {
  /** A mock whose acme organization holds the seeded token and `EXTRA_ROWS` unused ones. */
  const withTokens = () =>
    createMockTransport({ extraEnrollmentTokens: EXTRA_ROWS });

  test("shows the seeded token from one request of 200 rows", async () => {
    const { transport, state } = pagedTransport(
      "ListEnrollmentTokens",
      createMockTransport(),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    expect(await screen.findByText(/ose_91c2/, {}, WAIT)).toBeTruthy();
    // The absence of a second request cannot be awaited; give a request that
    // the console would wrongly send time to arrive.
    await settle(60);
    expect(state.tokens).toEqual([""]);
    expect(itemsOf("Enrollment tokens")).toHaveLength(1);
    expect(screen.queryByText(/Loading more enrollment tokens/)).toBeNull();
  });

  test("shows the empty state of an organization without tokens", async () => {
    const mock = createMockTransport();
    const { organization } = await createClient(
      OrganizationService,
      mock,
    ).createOrganization({ name: "Initech" });
    renderConsole(`/orgs/${organization!.organizationId}/enrollment-tokens`, {
      transport: mock,
    });
    expect(
      await screen.findByText("No enrollment tokens", {}, WAIT),
    ).toBeTruthy();
    expect(
      screen.queryByRole("list", { name: "Enrollment tokens" }),
    ).toBeNull();
  });

  test(
    "lists every page and revokes a new token that sorts after the first page",
    async () => {
      const { transport, state } = pagedTransport(
        "ListEnrollmentTokens",
        withTokens(),
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
      await waitForRows("Enrollment tokens", ALL_ROWS);
      expect(state.tokens).toHaveLength(2);
      expect(state.tokens[0]).toBe("");
      expect(state.tokens[1]).not.toBe("");

      // Create one more token in the console. It is the last row.
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Create token" }));
      });
      const form = await screen.findByRole(
        "dialog",
        { name: "Create an enrollment token" },
        WAIT,
      );
      await act(async () => {
        fireEvent.click(
          within(form).getByRole("button", { name: "Create token" }),
        );
      });
      const secret = await screen.findByRole(
        "dialog",
        { name: "Your new enrollment token" },
        WAIT,
      );
      await act(async () => {
        fireEvent.keyDown(secret, { key: "Escape" });
      });
      await waitFor(
        () =>
          expect(
            screen.queryByRole("dialog", { name: "Your new enrollment token" }),
          ).toBeNull(),
        WAIT,
      );
      const rows = await waitForRows("Enrollment tokens", ALL_ROWS + 1);
      const last = rows[rows.length - 1]!;
      expect(last.textContent).toContain("Not used");

      await act(async () => {
        fireEvent.click(within(last).getByRole("button", { name: /^Revoke/ }));
      });
      // Revoking asks for a confirmation first.
      const confirmation = await screen.findByRole("alertdialog", {}, WAIT);
      await act(async () => {
        fireEvent.click(
          within(confirmation).getByRole("button", { name: "Revoke token" }),
        );
      });
      await waitFor(() => {
        const after = itemsOf("Enrollment tokens");
        expect(after).toHaveLength(ALL_ROWS + 1);
        expect(after[after.length - 1]!.textContent).toContain("Revoked");
      }, WAIT);
      const after = itemsOf("Enrollment tokens");
      expect(
        within(after[after.length - 1]!).queryByRole("button", {
          name: /^Revoke/,
        }),
      ).toBeNull();
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );

  test(
    "keeps the loaded tokens and offers Try again when a later page fails",
    async () => {
      const { transport, state } = pagedTransport(
        "ListEnrollmentTokens",
        withTokens(),
      );
      state.failLaterPages = true;
      renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
      expect(
        await screen.findByText(
          "Some enrollment tokens are not shown yet.",
          {},
          WAIT,
        ),
      ).toBeTruthy();
      expect(itemsOf("Enrollment tokens")).toHaveLength(FIRST_PAGE_ROWS);
      const [, failedToken] = state.tokens;
      expect(failedToken).toBeTruthy();
      state.failLaterPages = false;
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      });
      await waitForRows("Enrollment tokens", ALL_ROWS);
      expect(state.tokens).toEqual(["", failedToken, failedToken]);
      await waitFor(
        () =>
          expect(
            screen.queryByText("Some enrollment tokens are not shown yet."),
          ).toBeNull(),
        WAIT,
      );
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );

  test(
    "shows no token of the previous organization after the organization changes",
    async () => {
      const mock = withTokens();
      const { organization } = await createClient(
        OrganizationService,
        mock,
      ).createOrganization({ name: "Initech" });
      renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, {
        transport: mock,
      });
      await waitForRows("Enrollment tokens", ALL_ROWS);
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("combobox", { name: "Organization" }, WAIT),
        );
      });
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("option", { name: "Initech" }, WAIT),
        );
      });
      await waitFor(
        () =>
          expect(window.location.pathname).toBe(
            `/orgs/${organization!.organizationId}/enrollment-tokens`,
          ),
        WAIT,
      );
      expect(
        await screen.findByText("No enrollment tokens", {}, WAIT),
      ).toBeTruthy();
      expect(
        screen.queryByRole("list", { name: "Enrollment tokens" }),
      ).toBeNull();
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );
});

describe("API keys list", () => {
  /** A mock whose acme organization holds the seeded key and `EXTRA_ROWS` active ones. */
  const withKeys = () => createMockTransport({ extraApiKeys: EXTRA_ROWS });

  test("shows the seeded key from one request of 200 rows", async () => {
    const { transport, state } = pagedTransport(
      "ListApiKeys",
      createMockTransport(),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    expect(await screen.findByText(/ci-runner/, {}, WAIT)).toBeTruthy();
    // The absence of a second request cannot be awaited; give a request that
    // the console would wrongly send time to arrive.
    await settle(60);
    expect(state.tokens).toEqual([""]);
    expect(itemsOf("API keys")).toHaveLength(1);
    expect(screen.queryByText(/Loading more API keys/)).toBeNull();
  });

  test("shows the empty state of an organization without keys", async () => {
    const mock = createMockTransport();
    const { organization } = await createClient(
      OrganizationService,
      mock,
    ).createOrganization({ name: "Initech" });
    renderConsole(`/orgs/${organization!.organizationId}/api-keys`, {
      transport: mock,
    });
    expect(await screen.findByText("No API keys", {}, WAIT)).toBeTruthy();
    expect(screen.queryByRole("list", { name: "API keys" })).toBeNull();
  });

  test(
    "lists every page and revokes a new key that sorts after the first page",
    async () => {
      const { transport, state } = pagedTransport("ListApiKeys", withKeys());
      renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
      await waitForRows("API keys", ALL_ROWS);
      expect(state.tokens).toHaveLength(2);
      expect(state.tokens[0]).toBe("");
      expect(state.tokens[1]).not.toBe("");

      // Create one more key in the console. It is the last row.
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Create API key" }));
      });
      const form = await screen.findByRole(
        "dialog",
        { name: "Create an API key" },
        WAIT,
      );
      await act(async () => {
        fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), {
          target: { value: "late-key" },
        });
      });
      await act(async () => {
        fireEvent.click(
          within(form).getByRole("button", { name: "Create key" }),
        );
      });
      const secret = await screen.findByRole(
        "dialog",
        { name: "Your new API key" },
        WAIT,
      );
      await act(async () => {
        fireEvent.keyDown(secret, { key: "Escape" });
      });
      await waitFor(
        () =>
          expect(
            screen.queryByRole("dialog", { name: "Your new API key" }),
          ).toBeNull(),
        WAIT,
      );
      const rows = await waitForRows("API keys", ALL_ROWS + 1);
      const last = rows[rows.length - 1]!;
      expect(last.textContent).toContain("late-key");

      await act(async () => {
        fireEvent.click(within(last).getByRole("button", { name: /^Revoke/ }));
      });
      const dialog = await screen.findByRole(
        "alertdialog",
        { name: "Revoke late-key?" },
        WAIT,
      );
      await act(async () => {
        fireEvent.click(
          within(dialog).getByRole("button", { name: "Revoke key" }),
        );
      });
      await waitFor(() => {
        const after = itemsOf("API keys");
        expect(after).toHaveLength(ALL_ROWS + 1);
        const revoked = after[after.length - 1]!;
        expect(revoked.textContent).toContain("late-key");
        expect(revoked.textContent).toContain("Revoked");
      }, WAIT);
      const after = itemsOf("API keys");
      expect(
        within(after[after.length - 1]!).queryByRole("button", {
          name: /^Revoke/,
        }),
      ).toBeNull();
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );

  test(
    "keeps the loaded keys and offers Try again when a later page fails",
    async () => {
      const { transport, state } = pagedTransport("ListApiKeys", withKeys());
      state.failLaterPages = true;
      renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
      expect(
        await screen.findByText("Some API keys are not shown yet.", {}, WAIT),
      ).toBeTruthy();
      expect(itemsOf("API keys")).toHaveLength(FIRST_PAGE_ROWS);
      const [, failedToken] = state.tokens;
      expect(failedToken).toBeTruthy();
      state.failLaterPages = false;
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      });
      await waitForRows("API keys", ALL_ROWS);
      expect(state.tokens).toEqual(["", failedToken, failedToken]);
      await waitFor(
        () =>
          expect(
            screen.queryByText("Some API keys are not shown yet."),
          ).toBeNull(),
        WAIT,
      );
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );

  test(
    "shows no key of the previous organization after the organization changes",
    async () => {
      const mock = withKeys();
      const { organization } = await createClient(
        OrganizationService,
        mock,
      ).createOrganization({ name: "Initech" });
      renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport: mock });
      await waitForRows("API keys", ALL_ROWS);
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("combobox", { name: "Organization" }, WAIT),
        );
      });
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("option", { name: "Initech" }, WAIT),
        );
      });
      await waitFor(
        () =>
          expect(window.location.pathname).toBe(
            `/orgs/${organization!.organizationId}/api-keys`,
          ),
        WAIT,
      );
      expect(await screen.findByText("No API keys", {}, WAIT)).toBeTruthy();
      expect(screen.queryByRole("list", { name: "API keys" })).toBeNull();
      expect(screen.queryByText(/key-0/)).toBeNull();
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );
});

describe.each([
  {
    path: "api-keys",
    method: "ListApiKeys",
    list: "API keys",
    noun: "API keys",
    create: "Create API key",
    options: { extraApiKeys: EXTRA_ROWS },
  },
  {
    path: "enrollment-tokens",
    method: "ListEnrollmentTokens",
    list: "Enrollment tokens",
    noun: "enrollment tokens",
    create: "Create token",
    options: { extraEnrollmentTokens: EXTRA_ROWS },
  },
])("$list terminal continuation errors", (fixture) => {
  test.each([
    [Code.Unauthenticated, "Your session ended. Sign in again."],
    [Code.NotFound, "It was not found, or you no longer have access to it."],
    [Code.PermissionDenied, "You do not have permission to do this."],
  ])(
    "replaces loaded rows and actions with the full notice for %s",
    async (code, message) => {
      const paged = pagedTransport(
        fixture.method,
        createMockTransport(fixture.options),
      );
      const { transport, calls } = holdingTransport(
        [fixture.method],
        () => true,
        paged.transport,
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/${fixture.path}`, { transport });
      await waitFor(() => expect(calls[fixture.method]).toHaveLength(1), WAIT);
      await act(async () => calls[fixture.method]![0]!.settle());
      await waitFor(() => expect(calls[fixture.method]).toHaveLength(2), WAIT);

      const list = screen.getByRole("list", { name: fixture.list });
      await act(async () =>
        fireEvent.click(
          within(list).getAllByRole("button", { name: /^Revoke/ })[0]!,
        ),
      );
      await screen.findByRole("alertdialog", {}, WAIT);
      await act(async () =>
        calls[fixture.method]![1]!.settle(
          new ConnectError("the page failed", code),
        ),
      );

      expect(await screen.findByText(message, {}, WAIT)).toBeTruthy();
      expect(screen.queryByRole("list", { name: fixture.list })).toBeNull();
      expect(screen.queryByRole("button", { name: /^Revoke/ })).toBeNull();
      expect(screen.queryByRole("alertdialog")).toBeNull();
      expect(
        screen.queryByText(`Some ${fixture.noun} are not shown yet.`),
      ).toBeNull();
      expect(screen.getByRole("button", { name: fixture.create })).toBeTruthy();
      if (code === Code.Unauthenticated) {
        expect(
          screen.getByRole("button", { name: "Sign in with Delino" }),
        ).toBeTruthy();
      }
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );
});

describe("device inventory", () => {
  test(
    "keeps exactly 200 unique rows when the final page is empty",
    async () => {
      const { transport, state } = pagedTransport(
        "ListDevices",
        createMockTransport({ extraDevices: 198 }),
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/devices`, { transport });
      const table = await screen.findByRole("table", {}, WAIT);
      await waitFor(() => {
        expect(within(table).getAllByRole("link")).toHaveLength(200);
        expect(state.tokens).toHaveLength(2);
        expect(screen.queryByText("Loading more devices")).toBeNull();
      }, WAIT);
      expect(
        new Set(
          within(table)
            .getAllByRole("link")
            .map((link) => link.getAttribute("href")),
        ).size,
      ).toBe(200);
      expect(state.tokens[0]).toBe("");
      expect(state.tokens[1]).not.toBe("");
    },
    LONG_LIST_TEST_TIMEOUT_MS,
  );
});
