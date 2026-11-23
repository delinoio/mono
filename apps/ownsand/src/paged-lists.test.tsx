// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

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
      state.failLaterPages = false;
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      });
      await waitForRows("Enrollment tokens", ALL_ROWS);
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
      state.failLaterPages = false;
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      });
      await waitForRows("API keys", ALL_ROWS);
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
