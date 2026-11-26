// SPDX-License-Identifier: Apache-2.0
import { setTimeout as delay } from "node:timers/promises";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import { create } from "@bufbuild/protobuf";
import { StrictMode } from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import {
  BaseImage,
  type DeviceLimitsInput,
  type Resources,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import {
  BillingService,
  type LinkBillingAccountRequest,
} from "@delino/ownsand-protos/v1/billing_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import type { AcceptInvitationRequest } from "@delino/ownsand-protos/v1/member_pb";
import { type Sandbox } from "@delino/ownsand-protos/v1/sandbox_pb";
import {
  ListOrganizationsResponseSchema,
  OrganizationService,
} from "@delino/ownsand-protos/v1/organization_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { focusManager, QueryClient } from "@tanstack/react-query";
import { toast } from "delino-ui/toast";
import { Console } from "./App.tsx";
import { createQueryClient, invalidateServices } from "./api.ts";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { AuthMode, type ConsoleConfig } from "./config.ts";
import { shortIdText } from "./components/ui.tsx";
import { formatTime } from "./format.ts";
import { pendingInvitation } from "./invitation.ts";
import { RouteId, RouterProvider, useRouter } from "./router.tsx";
import {
  createMockTransport,
  MOCK_BILLING_ACCOUNTS,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
  MOCK_USER,
  MockBillingState,
} from "./mock/mock-api.ts";
import { holdingTransport as holdPagedTransport } from "./test-support.tsx";
import {
  mockSandboxTransition,
  SandboxTransitionOutcome,
} from "../e2e/sandbox-transition-mock.ts";

/** The seeded Acme invitation, which the mock user cannot accept: already a member. */
const MOCK_ACME_INVITATION_SECRET = "osi_7f3a_unused";

const config = {
  apiOrigin: "https://api.test",
  authMode: AuthMode.Mock,
  logto: undefined,
};

function renderConsole(
  path: string,
  options: {
    signedIn?: boolean;
    transport?: Transport;
    config?: ConsoleConfig;
    queryClient?: QueryClient;
    /** The signed-in user; the mock user by default. */
    userId?: string;
    strict?: boolean;
    /** Reports a sign-in that failed before the redirect. */
    signInFailed?: boolean;
  } = {},
) {
  window.history.replaceState(null, "", path);
  const signIns: number[] = [];
  const signedIn = options.signedIn ?? true;
  const auth: Auth = {
    isLoading: false,
    isAuthenticated: signedIn,
    userId: signedIn ? (options.userId ?? MOCK_USER) : undefined,
    signIn: async () => {
      signIns.push(1);
    },
    signInFailed: options.signInFailed ?? false,
    signOut: async () => {},
    getAccessToken: async () => "token",
  };
  const queryClient =
    options.queryClient ??
    new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
  const tree = (
    <AuthContextProvider value={auth}>
      <Console
        config={options.config ?? config}
        transport={options.transport ?? createMockTransport()}
        queryClient={queryClient}
      />
    </AuthContextProvider>
  );
  render(options.strict ? <StrictMode>{tree}</StrictMode> : tree);
  return { signIns };
}

async function settle(ms = 30) {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  // The toaster and its screen reader announcers are module state outside
  // the rendered tree: a toast or an announcement of one test must not
  // appear in the document of the next one.
  toast.dismiss();
  for (const announcer of document.querySelectorAll(
    "[data-ui-sdk-announcer]",
  )) {
    announcer.textContent = "";
  }
});

describe("organizations", () => {
  test("lists the caller's organizations with the caller's role", async () => {
    renderConsole("/");
    expect(
      await screen.findByRole("link", { name: /Acme Robotics/ }),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: /Northwind Labs/ }).textContent,
    ).toContain("Member");
  });

  /**
   * A mock control plane that holds 50 organizations besides the seeded
   * ones, and the page sizes of the ListOrganizations requests that reach it.
   * The mock returns 50 organizations per page by default, so only the
   * requested page size tells a request for 200 from one for 50.
   */
  async function manyOrganizations() {
    const mock = createMockTransport();
    const client = createClient(OrganizationService, mock);
    for (let i = 0; i < 50; i++) {
      await client.createOrganization({ name: `Extra ${i}` });
    }
    const pageSizes: number[] = [];
    const transport: Transport = {
      ...mock,
      unary: (method, ...rest) => {
        if (method.name === "ListOrganizations") {
          pageSizes.push((rest[3] as { pageSize: number }).pageSize);
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, pageSizes };
  }

  test("requests pages of 200 organizations on the organizations page", async () => {
    const { transport, pageSizes } = await manyOrganizations();
    renderConsole("/", { transport });
    expect(await screen.findByRole("link", { name: /Extra 49/ })).toBeTruthy();
    expect(pageSizes.length).toBeGreaterThan(0);
    expect(pageSizes.every((size) => size === 200)).toBe(true);
  });

  // The organizations page mounts the top bar without an organization, so it
  // never renders the switcher: this case opens an organization route, where
  // the switcher is the only consumer that the test can observe.
  test("requests pages of 200 organizations in the switcher", async () => {
    const { transport, pageSizes } = await manyOrganizations();
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    expect(
      await screen.findByRole("option", { name: "Extra 49" }),
    ).toBeTruthy();
    expect(pageSizes.length).toBeGreaterThan(0);
    expect(pageSizes.every((size) => size === 200)).toBe(true);
  });

  /**
   * A control plane whose ListOrganizations returns the seeded organizations
   * of the mock one page each, with the token `page-<n>` that leads to page
   * n. `pages[i]` lists the organization IDs of page i + 1. `failPage` is
   * the 1-based page that fails with `unavailable` while it is set.
   */
  async function pagedOrganizations(pages: string[][]) {
    const mock = createMockTransport();
    const real = await createClient(
      OrganizationService,
      mock,
    ).listOrganizations({ pageSize: 200 });
    const state = {
      tokens: [] as string[],
      failPage: undefined as number | undefined,
    };
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name !== "ListOrganizations") {
          return mock.unary(method, ...rest);
        }
        const token = (rest[3] as { pageToken: string }).pageToken;
        state.tokens.push(token);
        const page = token === "" ? 1 : Number(token.slice("page-".length));
        if (state.failPage === page) {
          throw new ConnectError("the page failed", Code.Unavailable);
        }
        // Only the envelope of the response matters; the rows come from `real`.
        const res = await mock.unary(
          method,
          rest[0],
          rest[1],
          rest[2],
          { pageSize: 200 } as never,
          rest[4],
        );
        return {
          ...res,
          message: create(ListOrganizationsResponseSchema, {
            organizations: real.organizations.filter((o) =>
              pages[page - 1]!.includes(o.organizationId),
            ),
            nextPageToken: page < pages.length ? `page-${page + 1}` : "",
          }),
        } as unknown as typeof res;
      },
    };
    return { transport, state };
  }

  test("follows the continuation token and lists a later-page organization with its role", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    renderConsole("/", { transport });
    const later = await screen.findByRole("link", { name: /Northwind Labs/ });
    expect(later.textContent).toContain("Member");
    expect(
      screen.getByRole("link", { name: /Acme Robotics/ }).textContent,
    ).toContain("Owner");
    // The first request has no token, the second one sends the token of the
    // first response, and the page and the switcher share both requests.
    expect(state.tokens).toEqual(["", "page-2"]);
    expect(screen.queryByText("Loading more organizations")).toBeNull();
  });

  test("offers a later-page organization in the switcher", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    expect(
      await screen.findByRole("option", { name: "Northwind Labs" }),
    ).toBeTruthy();
    expect(state.tokens).toEqual(["", "page-2"]);
    await act(async () => {
      fireEvent.click(screen.getByRole("option", { name: "Northwind Labs" }));
    });
    await settle(60);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.northwind}/sandboxes`,
    );
    // The member of Northwind does not see the admin sections.
    const nav = await screen.findByRole("navigation", { name: "Organization" });
    expect(nav.textContent).not.toContain("API keys");
  });

  test("stops at a final empty page after a full page", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme, MOCK_IDS.northwind],
      [],
    ]);
    renderConsole("/", { transport });
    expect(
      await screen.findByRole("link", { name: /Northwind Labs/ }),
    ).toBeTruthy();
    await settle(60);
    expect(state.tokens).toEqual(["", "page-2"]);
    expect(screen.queryByText("Loading more organizations")).toBeNull();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  });

  test("requests no second page when the first response has no token", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme, MOCK_IDS.northwind],
    ]);
    renderConsole("/", { transport });
    await screen.findByRole("link", { name: /Northwind Labs/ });
    await settle(60);
    expect(state.tokens).toEqual([""]);
  });

  test("shows an incomplete list with a retry when a later page fails", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    state.failPage = 2;
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    // The switcher says that it is incomplete and offers the retry.
    const retryOption = await screen.findByRole("option", {
      name: /Some organizations did not load/,
    });
    expect(screen.queryByRole("option", { name: "Northwind Labs" })).toBeNull();
    await act(async () => {
      fireEvent.keyDown(document.activeElement ?? document.body, {
        key: "Escape",
      });
    });
    await settle(60);
    // A failed page is not requested again without a retry.
    expect(state.tokens).toEqual(["", "page-2"]);
    state.failPage = undefined;
    await act(async () => {
      fireEvent.click(retryOption);
    });
    await settle(60);
    expect(state.tokens).toEqual(["", "page-2", "page-2"]);
    await act(async () => {
      fireEvent.click(screen.getByRole("combobox", { name: "Organization" }));
    });
    expect(
      await screen.findByRole("option", { name: "Northwind Labs" }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("option", { name: /Some organizations did not load/ }),
    ).toBeNull();
  });

  test("hides loaded organizations in the switcher and list after a terminal continuation error", async () => {
    const paged = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    const { transport, calls } = holdPagedTransport(
      ["ListOrganizations"],
      () => true,
      paged.transport,
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await waitFor(() => expect(calls.ListOrganizations).toHaveLength(1));
    await act(async () => calls.ListOrganizations![0]!.settle());
    const switcher = await screen.findByRole("combobox", {
      name: "Organization",
    });
    await act(async () => fireEvent.click(switcher));
    expect(
      await screen.findByRole("option", { name: "Acme Robotics" }),
    ).toBeTruthy();

    await act(async () =>
      calls.ListOrganizations![1]!.settle(
        new ConnectError("the page failed", Code.Unauthenticated),
      ),
    );
    await waitFor(() => expect(switcher.isConnected).toBe(false));
    expect(screen.queryByRole("option", { name: "Acme Robotics" })).toBeNull();
    expect(
      screen.queryByRole("option", { name: /Some organizations did not load/ }),
    ).toBeNull();
    expect(
      screen.getByRole("navigation", { name: "Organization" }),
    ).toBeTruthy();

    await act(async () =>
      fireEvent.click(screen.getByRole("link", { name: "OwnSand" })),
    );
    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    expect(
      screen.queryByRole("list", { name: "Your organizations" }),
    ).toBeNull();
    expect(
      screen.queryByText("Some organizations are not shown yet."),
    ).toBeNull();
    expect(
      screen.getByRole("button", { name: "Sign in with Delino" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Create organization" }),
    ).toBeTruthy();
    expect(calls.ListOrganizations).toHaveLength(2);
  });

  test("keeps the loaded organizations and offers Try again on the list when a later page fails", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    state.failPage = 2;
    renderConsole("/", { transport });
    expect(
      await screen.findByText("Some organizations are not shown yet."),
    ).toBeTruthy();
    expect(screen.queryByText(/The last refresh failed/)).toBeNull();
    expect(screen.getByRole("link", { name: /Acme Robotics/ })).toBeTruthy();
    expect(screen.queryByRole("link", { name: /Northwind Labs/ })).toBeNull();
    state.failPage = undefined;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    expect(
      await screen.findByRole("link", { name: /Northwind Labs/ }),
    ).toBeTruthy();
    expect(
      screen.queryByText("Some organizations are not shown yet."),
    ).toBeNull();
    expect(state.tokens).toEqual(["", "page-2", "page-2"]);
  });

  /**
   * Moves the focus away from the window and back, as a user does when they
   * switch to another tab and return. React Query refetches a stale entry
   * that has an observer when the focus returns.
   */
  async function refocusWindow() {
    try {
      await act(async () => {
        focusManager.setFocused(false);
        focusManager.setFocused(true);
      });
      await settle(60);
    } finally {
      focusManager.setFocused(undefined);
    }
  }

  test("does not request the failed page again when the window regains focus", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    state.failPage = 2;
    // The test client keeps no data fresh, so the focus refetches the entry
    // unless the hook pauses it.
    renderConsole("/", { transport });
    expect(
      await screen.findByText("Some organizations are not shown yet."),
    ).toBeTruthy();
    // The control plane would answer page 2 now, so a request that the
    // console sends on its own would show Northwind Labs.
    state.failPage = undefined;
    await refocusWindow();
    expect(state.tokens).toEqual(["", "page-2"]);
    expect(screen.getByRole("link", { name: /Acme Robotics/ })).toBeTruthy();
    expect(screen.queryByRole("link", { name: /Northwind Labs/ })).toBeNull();
    expect(
      screen.getByText("Some organizations are not shown yet."),
    ).toBeTruthy();
    // Only the explicit retry requests the page, and it continues from the
    // last loaded page.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    expect(
      await screen.findByRole("link", { name: /Northwind Labs/ }),
    ).toBeTruthy();
    expect(state.tokens).toEqual(["", "page-2", "page-2"]);
  });

  test("retries the failed page from the switcher after the window regained focus", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    state.failPage = 2;
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    const retryOption = await screen.findByRole("option", {
      name: /Some organizations did not load/,
    });
    state.failPage = undefined;
    await refocusWindow();
    expect(state.tokens).toEqual(["", "page-2"]);
    expect(
      screen.getByRole("option", { name: /Some organizations did not load/ }),
    ).toBeTruthy();
    await act(async () => {
      fireEvent.click(retryOption);
    });
    await settle(60);
    expect(state.tokens).toEqual(["", "page-2", "page-2"]);
    await act(async () => {
      fireEvent.click(screen.getByRole("combobox", { name: "Organization" }));
    });
    expect(
      await screen.findByRole("option", { name: "Northwind Labs" }),
    ).toBeTruthy();
  });

  test("refreshes the organizations from the first page after an invalidation that follows a failed page", async () => {
    const { transport, state } = await pagedOrganizations([
      [MOCK_IDS.acme],
      [MOCK_IDS.northwind],
    ]);
    state.failPage = 2;
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    renderConsole("/", { transport, queryClient });
    await screen.findByText("Some organizations are not shown yet.");
    state.failPage = undefined;
    await refocusWindow();
    expect(state.tokens).toEqual(["", "page-2"]);
    // A mutation of the OrganizationService invalidates the entry; the
    // refetch starts at the first page and then follows the token.
    await act(async () => {
      await invalidateServices(queryClient, OrganizationService);
    });
    expect(
      await screen.findByRole("link", { name: /Northwind Labs/ }),
    ).toBeTruthy();
    expect(state.tokens).toEqual(["", "page-2", "", "page-2"]);
    expect(
      screen.queryByText("Some organizations are not shown yet."),
    ).toBeNull();
  });

  test("opens an organization's sandboxes and hides ended sandboxes by default", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    expect(
      await screen.findByRole("heading", { name: "Sandboxes", level: 1 }),
    ).toBeTruthy();
    await screen.findByText("Running");
    expect(screen.queryByText("Deleted")).toBeNull();
    expect(screen.getByText(/Unreachable \(last running\)/)).toBeTruthy();
  });
});

describe("roles", () => {
  test("a member does not see admin sections", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/sandboxes`);
    const nav = await screen.findByRole("navigation", { name: "Organization" });
    await screen.findByRole("link", { name: "Sandboxes" });
    expect(nav.textContent).not.toContain("API keys");
    expect(nav.textContent).not.toContain("Enrollment tokens");
  });

  test("a member who opens an admin section sees no admin actions", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/api-keys`);
    expect(
      await screen.findByText("You do not have permission to open this page."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create API key" })).toBeNull();
    expect(screen.getByRole("link", { name: "Go to sandboxes" })).toBeTruthy();
  });

  test("an owner sees every section", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    await screen.findByRole("link", { name: "API keys" });
    expect(
      screen.getByRole("link", { name: "Enrollment tokens" }),
    ).toBeTruthy();
  });
});

describe("API keys", () => {
  test("focuses the name, shows the new key once and closes with Escape", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`);
    await screen.findByText(/ci-runner/);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create API key" }));
    });
    await settle(60);
    const input = screen.getByRole("textbox", { name: "Name" });
    expect(document.activeElement).toBe(input);
    await act(async () => {
      fireEvent.input(input, { target: { value: "deploy-bot" } });
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create key" }));
    });
    await settle(60);
    const dialog = await screen.findByRole("dialog", {
      name: "Your new API key",
    });
    const value = dialog.querySelector("code")!.textContent!;
    expect(value).toMatch(/^osk_[0-9a-f]{32}$/);
    expect(dialog.textContent).toContain("appears only once");
    // The secret never reaches browser storage.
    const stored = JSON.stringify({
      ...window.sessionStorage,
      ...window.localStorage,
    });
    expect(stored).not.toContain(value);
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByText(value)).toBeNull();
    expect(await screen.findByText(/deploy-bot/)).toBeTruthy();
  });
});

describe("dialog focus return", () => {
  // A dialog with an autofocused input records its opener only after React
  // autofocus has moved focus, so it used to leave focus on the page body.
  // These tests press the opener with real focus first, as a keyboard user does.
  async function press(opener: HTMLElement) {
    await act(async () => {
      opener.focus();
      fireEvent.click(opener);
    });
    await settle(60);
  }

  async function escape() {
    await act(async () => {
      fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
    });
    await settle(60);
  }

  async function cancel(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    });
    await settle(60);
  }

  test("Create organization returns focus to its button after Escape and Cancel", async () => {
    renderConsole("/");
    await screen.findByRole("link", { name: /Acme Robotics/ });
    const opener = screen.getByRole("button", { name: "Create organization" });

    await press(opener);
    const dialog = screen.getByRole("dialog", {
      name: "Create an organization",
    });
    expect(document.activeElement).toBe(
      within(dialog).getByRole("textbox", { name: "Organization name" }),
    );
    await escape();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);

    await press(opener);
    await cancel(
      screen.getByRole("dialog", { name: "Create an organization" }),
    );
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  test("Create API key returns focus to its button after Escape and Cancel", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`);
    await screen.findByText(/ci-runner/);
    const opener = screen.getByRole("button", { name: "Create API key" });

    await press(opener);
    const dialog = screen.getByRole("dialog", { name: "Create an API key" });
    expect(document.activeElement).toBe(
      within(dialog).getByRole("textbox", { name: "Name" }),
    );
    await escape();
    expect(document.activeElement).toBe(opener);

    await press(opener);
    await cancel(screen.getByRole("dialog", { name: "Create an API key" }));
    expect(document.activeElement).toBe(opener);
  });

  test("the one-time API key dialog returns focus to Create API key after Done and Escape", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`);
    await screen.findByText(/ci-runner/);
    const opener = screen.getByRole("button", { name: "Create API key" });

    for (const close of ["Done", "Escape"]) {
      await press(opener);
      const form = screen.getByRole("dialog", { name: "Create an API key" });
      await act(async () => {
        fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), {
          target: { value: `key-${close}` },
        });
      });
      await act(async () => {
        fireEvent.click(
          within(form).getByRole("button", { name: "Create key" }),
        );
      });
      await settle(60);
      const shown = await screen.findByRole("dialog", {
        name: "Your new API key",
      });
      if (close === "Done") {
        await act(async () => {
          fireEvent.click(within(shown).getByRole("button", { name: "Done" }));
        });
        await settle(60);
      } else {
        await escape();
      }
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(document.activeElement).toBe(opener);
    }
  });

  test("Make template returns focus to its button after Escape and Cancel", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/snapshots`);
    const opener = (
      await screen.findAllByRole("button", { name: /^Make template/ })
    )[0]!;

    await press(opener);
    const dialog = screen.getByRole("dialog", { name: "Make a template" });
    expect(document.activeElement).toBe(
      within(dialog).getByRole("textbox", { name: "Template name" }),
    );
    await escape();
    expect(document.activeElement).toBe(opener);

    await press(opener);
    await cancel(screen.getByRole("dialog", { name: "Make a template" }));
    expect(document.activeElement).toBe(opener);
  });

  test("Invite someone, a dialog without an autofocused input, still returns focus to its button", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
    const opener = await screen.findByRole("button", {
      name: "Invite someone",
    });

    await press(opener);
    screen.getByRole("dialog", { name: "Invite someone" });
    await escape();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
  });
});

describe("confirmation dialog errors", () => {
  /** A transport whose RevokeApiKey fails for the first `failures` calls. */
  function revokeTransport(failures: number) {
    const mock = createMockTransport();
    const state = { calls: 0 };
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "RevokeApiKey") {
          state.calls += 1;
          if (state.calls <= failures) {
            throw new ConnectError("revoke failed", Code.Unavailable);
          }
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, state };
  }

  /** The error notice inside a dialog; the live-region announcer also has the alert role. */
  const noticeIn = (dialog: HTMLElement) =>
    dialog.querySelector(".notice-error");

  async function openRevoke() {
    await act(async () => {
      fireEvent.click(screen.getAllByRole("button", { name: /Revoke/ })[0]);
    });
    await settle();
    return screen.findByRole("alertdialog");
  }

  async function confirmRevoke() {
    const dialog = await screen.findByRole("alertdialog");
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Revoke key" }),
      );
    });
    await settle(60);
  }

  test("a failed action keeps the dialog open with a notice that a retry removes", async () => {
    const { transport, state } = revokeTransport(1);
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
    await openRevoke();
    await confirmRevoke();
    expect(noticeIn(screen.getByRole("alertdialog"))).toBeTruthy();
    // The retry removes the old notice while it runs and closes on success.
    await confirmRevoke();
    expect(state.calls).toBe(2);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(document.querySelector(".notice-error")).toBeNull();
    expect(document.querySelector(".notice-error")).toBeNull();
  });

  test("a notice does not survive Cancel and reopening", async () => {
    const { transport } = revokeTransport(1);
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
    await openRevoke();
    await confirmRevoke();
    expect(noticeIn(screen.getByRole("alertdialog"))).toBeTruthy();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    });
    await settle(60);
    await screen.findByText(/ci-runner/);
    const dialog = await openRevoke();
    expect(noticeIn(dialog)).toBeNull();
  });

  test("a failure that arrives after Escape does not show on the next opening", async () => {
    const mock = createMockTransport();
    let release: (() => void) | undefined;
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "RevokeApiKey") {
          await new Promise<void>((resolve) => {
            release = resolve;
          });
          throw new ConnectError("revoke failed", Code.Unavailable);
        }
        return mock.unary(method, ...rest);
      },
    };
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
    const dialog = await openRevoke();
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Revoke key" }),
      );
    });
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await act(async () => {
      release?.();
    });
    await settle(60);
    const reopened = await openRevoke();
    expect(noticeIn(reopened)).toBeNull();
  });
});

describe("confirmation dialog requests", () => {
  /** One RevokeApiKey request that waits until the test settles it. */
  interface RevokeCall {
    apiKeyId: string;
    /** Fails the request when `error` is set and lets it succeed otherwise. */
    settle(error?: ConnectError): void;
  }

  /** A transport that holds every RevokeApiKey request until the test settles it. */
  function gatedRevokeTransport() {
    const mock = createMockTransport();
    const calls: RevokeCall[] = [];
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "RevokeApiKey") {
          const input = rest[3] as { apiKeyId: string };
          const error = await new Promise<ConnectError | undefined>(
            (resolve) => {
              calls.push({ apiKeyId: input.apiKeyId, settle: resolve });
            },
          );
          if (error !== undefined) throw error;
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, calls };
  }

  /** The error notice inside a dialog; the live-region announcer also has the alert role. */
  const noticeIn = (dialog: HTMLElement) =>
    dialog.querySelector(".notice-error");

  /** Renders the API keys page with the seeded ci-runner key and a newer deploy-bot key. */
  async function renderKeys(transport: Transport) {
    await createClient(ApiKeyService, transport).createApiKey({
      organizationId: MOCK_IDS.acme,
      name: "deploy-bot",
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
  }

  async function openRevokeOf(name: string) {
    const row = screen.getByText(new RegExp(`^${name} `)).closest("li")!;
    await act(async () => {
      fireEvent.click(within(row).getByRole("button", { name: /^Revoke/ }));
    });
    await settle();
    return screen.findByRole("alertdialog", { name: `Revoke ${name}?` });
  }

  const confirmButton = (dialog: HTMLElement) =>
    within(dialog).getByRole("button", {
      name: "Revoke key",
    }) as HTMLButtonElement;

  async function press(button: HTMLElement) {
    await act(async () => {
      fireEvent.click(button);
    });
    await settle();
  }

  async function settleCall(
    call: RevokeCall | undefined,
    error?: ConnectError,
  ) {
    await act(async () => {
      call?.settle(error);
    });
    await settle(60);
  }

  async function pressEscape(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
  }

  test("two clicks before the request settles send it once and the button is disabled", async () => {
    const { transport, calls } = gatedRevokeTransport();
    await renderKeys(transport);
    const dialog = await openRevokeOf("ci-runner");
    const button = confirmButton(dialog);
    await press(button);
    await press(button);
    expect(calls.length).toBe(1);
    expect(button.disabled).toBe(true);
    await settleCall(calls[0]);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(calls.length).toBe(1);
  });

  test("after a failure the button is enabled again, shows the error and sends a new request", async () => {
    const { transport, calls } = gatedRevokeTransport();
    await renderKeys(transport);
    const dialog = await openRevokeOf("ci-runner");
    await press(confirmButton(dialog));
    await settleCall(
      calls[0],
      new ConnectError("revoke failed", Code.Unavailable),
    );
    expect(noticeIn(dialog)).toBeTruthy();
    expect(confirmButton(dialog).disabled).toBe(false);
    await press(confirmButton(dialog));
    expect(calls.length).toBe(2);
    // The retry removes the notice while it runs.
    expect(noticeIn(dialog)).toBeNull();
    await settleCall(calls[1]);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  test("a late success of a dismissed confirmation leaves the next confirmation open", async () => {
    const { transport, calls } = gatedRevokeTransport();
    await renderKeys(transport);
    const first = await openRevokeOf("ci-runner");
    await press(confirmButton(first));
    await pressEscape(first);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const second = await openRevokeOf("deploy-bot");
    expect(confirmButton(second).disabled).toBe(false);
    await settleCall(calls[0]);
    const open = screen.getByRole("alertdialog");
    expect(open.textContent).toContain("Revoke deploy-bot?");
    expect(confirmButton(open).disabled).toBe(false);
    // The next confirmation still works for its own target.
    await press(confirmButton(open));
    expect(calls.length).toBe(2);
    expect(calls[1].apiKeyId).not.toBe(calls[0].apiKeyId);
    await settleCall(calls[1]);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  test("a late failure of a dismissed confirmation does not show on the next opening, also for the same target", async () => {
    const { transport, calls } = gatedRevokeTransport();
    await renderKeys(transport);
    const first = await openRevokeOf("ci-runner");
    await press(confirmButton(first));
    await press(within(first).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await openRevokeOf("ci-runner");
    await settleCall(
      calls[0],
      new ConnectError("revoke failed", Code.Unavailable),
    );
    const open = screen.getByRole("alertdialog");
    expect(noticeIn(open)).toBeNull();
    expect(confirmButton(open).disabled).toBe(false);
  });

  test("a failure that arrives while the dialog is open shows its own notice", async () => {
    const { transport, calls } = gatedRevokeTransport();
    await renderKeys(transport);
    const dialog = await openRevokeOf("ci-runner");
    await press(confirmButton(dialog));
    await settleCall(
      calls[0],
      new ConnectError("revoke failed", Code.Unavailable),
    );
    expect(noticeIn(screen.getByRole("alertdialog"))).toBeTruthy();
  });

  test("Escape and Cancel still dismiss a confirmation that is pending", async () => {
    const { transport, calls } = gatedRevokeTransport();
    await renderKeys(transport);
    const first = await openRevokeOf("ci-runner");
    await press(confirmButton(first));
    await pressEscape(first);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const second = await openRevokeOf("deploy-bot");
    await press(confirmButton(second));
    await press(within(second).getByRole("button", { name: "Cancel" }));
    await settle(60);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(calls.length).toBe(2);
  });
});

describe("form dialog errors", () => {
  /**
   * A transport whose CreateApiKey waits for `release()` and then fails for
   * the first `failures` calls; without a `gate`, it fails at once.
   */
  function createKeyTransport(failures: number, gate = false) {
    const mock = createMockTransport();
    const state = {
      calls: 0,
      release: undefined as (() => void) | undefined,
    };
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "CreateApiKey") {
          state.calls += 1;
          if (gate) {
            await new Promise<void>((resolve) => {
              state.release = resolve;
            });
          }
          if (state.calls <= failures) {
            throw new ConnectError("create failed", Code.Unavailable);
          }
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, state };
  }

  /** The error notice inside a dialog; the live-region announcer also has the alert role. */
  const noticeIn = (dialog: HTMLElement) =>
    dialog.querySelector(".notice-error");

  async function openCreate() {
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create API key" }));
    });
    await settle(60);
    return screen.findByRole("dialog", { name: "Create an API key" });
  }

  async function submitCreate(dialog: HTMLElement, name = "deploy-bot") {
    await act(async () => {
      fireEvent.input(within(dialog).getByRole("textbox", { name: "Name" }), {
        target: { value: name },
      });
    });
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Create key" }),
      );
    });
  }

  test("a failure while the dialog is open shows its notice and a retry removes it", async () => {
    const { transport, state } = createKeyTransport(1);
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
    const dialog = await openCreate();
    await submitCreate(dialog);
    await settle(60);
    expect(noticeIn(dialog)).toBeTruthy();
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Create key" }),
      );
    });
    await settle(60);
    expect(state.calls).toBe(2);
    expect(noticeIn(dialog)).toBeNull();
    expect(
      await screen.findByRole("dialog", { name: "Your new API key" }),
    ).toBeTruthy();
  });

  test("a notice does not survive Cancel and reopening", async () => {
    const { transport } = createKeyTransport(1);
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
    const dialog = await openCreate();
    await submitCreate(dialog);
    await settle(60);
    expect(noticeIn(dialog)).toBeTruthy();
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    });
    await settle(60);
    expect(
      screen.queryByRole("dialog", { name: "Create an API key" }),
    ).toBeNull();
    const reopened = await openCreate();
    expect(noticeIn(reopened)).toBeNull();
  });

  test("a failure that arrives after Escape does not show on the next opening", async () => {
    const { transport, state } = createKeyTransport(1, true);
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/ci-runner/);
    const dialog = await openCreate();
    await submitCreate(dialog);
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(
      screen.queryByRole("dialog", { name: "Create an API key" }),
    ).toBeNull();
    // The request fails only now, while the dialog is closed.
    await act(async () => {
      state.release?.();
    });
    await settle(60);
    const reopened = await openCreate();
    expect(noticeIn(reopened)).toBeNull();
  });
});

describe("devices", () => {
  test("shows the overcommitted and effective vCPU limit", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices`);
    expect(await screen.findByText("10 of 24 (16 × 1.5)")).toBeTruthy();
  });

  test("the device page shows the effective vCPU limit for admins", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`);
    const effective = await screen.findByRole("status", {
      name: "Effective vCPU limit",
    });
    await settle();
    expect(effective.textContent).toContain("24");
    expect(screen.getByRole("button", { name: "Save limits" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove device" })).toBeTruthy();
  });

  test("shows an effective vCPU limit above the int32 range exactly", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`);
    const vcpu = await screen.findByLabelText("vCPU limit");
    await settle();
    await act(async () => {
      fireEvent.input(vcpu, { target: { value: "2000000000" } });
      fireEvent.blur(vcpu);
    });
    await settle();
    // 2000000000 vCPUs at the existing 150 percent exceed the int32 range.
    const effective = screen.getByRole("status", {
      name: "Effective vCPU limit",
    });
    expect(effective.textContent).toContain("3000000000");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
    });
    await settle(60);
    await act(async () => {
      fireEvent.click(screen.getByRole("link", { name: "Devices" }));
    });
    expect(
      await screen.findByText("10 of 3000000000 (2000000000 × 1.5)"),
    ).toBeTruthy();
  });

  test("a removed device disappears from the device list", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Remove device" }),
      );
    });
    const dialog = await screen.findByRole("alertdialog");
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Remove device" }),
      );
    });
    await settle(100);
    expect(await screen.findByText("studio-mac")).toBeTruthy();
    expect(screen.queryByText("build-box-1")).toBeNull();
  });
});

describe("cached lists after a mutation that changes another service", () => {
  /** The console with the production query client, whose staleTime is 10 s. */
  function renderCounting(
    path: string,
    mock: Transport = createMockTransport(),
  ) {
    const counted = countingTransport(mock);
    renderConsole(path, {
      transport: counted.transport,
      queryClient: createQueryClient(),
    });
    return counted;
  }

  const open = async (name: string | RegExp) => {
    await act(async () => {
      fireEvent.click(await screen.findByRole("link", { name }));
    });
    await settle(60);
  };

  const confirmRemoveDevice = async () => {
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Remove device" }),
      );
    });
    const dialog = await screen.findByRole("alertdialog");
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Remove device" }),
      );
    });
    await settle(100);
  };

  test("a removed device's sandboxes, snapshots and templates leave the fresh cached lists", async () => {
    const { calls } = renderCounting(`/orgs/${MOCK_IDS.acme}/snapshots`);
    expect(await screen.findByText("node-24-toolchain")).toBeTruthy();
    await open("Sandboxes");
    await open("Devices");
    const before = { ...calls };
    await open("build-box-1");
    await confirmRemoveDevice();
    // The device list is the page that the removal returns to.
    expect(await screen.findByText("studio-mac")).toBeTruthy();
    // The cached lists are inactive here and refetch when a page shows them
    // again, although their 10 s staleTime has not passed.
    await open("Snapshots");
    expect(await screen.findByText("No snapshots")).toBeTruthy();
    expect(screen.queryByText("node-24-toolchain")).toBeNull();
    expect(calls.ListSnapshots).toBeGreaterThan(before.ListSnapshots ?? 0);
    expect(calls.ListTemplates).toBeGreaterThan(before.ListTemplates ?? 0);
    await open("Sandboxes");
    expect(calls.ListSandboxes).toBeGreaterThan(before.ListSandboxes ?? 0);
    expect(screen.queryByText("Running")).toBeNull();
  });

  test("a created sandbox invalidates the device queries", async () => {
    const { calls } = renderCounting(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Create sandbox" }),
      );
    });
    await settle(60);
    const dialog = await screen.findByRole("dialog", {
      name: "Create a sandbox",
    });
    const before = calls.ListDevices ?? 0;
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(100);
    expect(calls.CreateSandbox).toBe(1);
    expect(calls.ListDevices).toBeGreaterThan(before);
  });

  test("a deleted sandbox invalidates the device queries", async () => {
    const { calls } = renderCounting(
      `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`,
    );
    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    });
    const dialog = await screen.findByRole("alertdialog");
    await settle(60);
    const before = calls.ListDevices ?? 0;
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Delete sandbox" }),
      );
    });
    await settle(100);
    expect(calls.DeleteSandbox).toBe(1);
    expect(calls.ListDevices).toBeGreaterThan(before);
  });

  test("a sandbox that is still deleting invalidates the device queries once its state settles", async () => {
    const mock = createMockTransport();
    let polls = 0;
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        const response = await mock.unary(method, ...rest);
        // DeleteSandbox and the first read after it report DELETING, as the
        // control plane does when the agent has not finished. The response
        // is a copy, so the mock's own state stays DELETED.
        const { sandbox } = response.message as { sandbox?: Sandbox };
        if (
          (method.name === "DeleteSandbox" ||
            (method.name === "GetSandbox" && polls++ === 1)) &&
          sandbox
        ) {
          sandbox.state = SandboxState.DELETING;
        }
        return response;
      },
    };
    const { calls } = renderCounting(
      `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`,
      transport,
    );
    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    });
    const dialog = await screen.findByRole("alertdialog");
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Delete sandbox" }),
      );
    });
    await settle(100);
    const afterDelete = calls.ListDevices ?? 0;
    // The page polls every 3 s while the sandbox is DELETING.
    await settle(3_500);
    expect(calls.ListDevices).toBeGreaterThan(afterDelete);
  }, 15_000);

  test("pause, resume and snapshot do not refetch the device queries", async () => {
    const { calls } = renderCounting(
      `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`,
    );
    const press = async (name: string) => {
      await act(async () => {
        fireEvent.click(await screen.findByRole("button", { name }));
      });
      await settle(100);
    };
    await screen.findByRole("button", { name: "Pause" });
    await settle(60);
    const before = calls.ListDevices;
    await press("Pause");
    await press("Resume");
    await press("Take snapshot");
    expect(calls.PauseSandbox).toBe(1);
    expect(calls.ResumeSandbox).toBe(1);
    expect(calls.CreateSnapshot).toBe(1);
    expect(calls.ListDevices).toBe(before);
  });

  describe("Take snapshot while a request is pending", () => {
    /** A transport that holds each CreateSnapshot response until released. */
    function holdingSnapshots() {
      const mock = createMockTransport();
      const held: Array<{ release(error?: ConnectError): void }> = [];
      const transport: Transport = {
        ...mock,
        async unary(method, ...rest) {
          if (method.name !== "CreateSnapshot") {
            return mock.unary(method, ...rest);
          }
          await new Promise<void>((resolve, reject) => {
            held.push({
              release: (error) => (error ? reject(error) : resolve()),
            });
          });
          return mock.unary(method, ...rest);
        },
      };
      return { transport, held };
    }

    const take = async () => {
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("button", { name: "Take snapshot" }),
        );
      });
    };

    for (const outcome of ["succeeds", "fails"] as const) {
      test(`sends one request for repeated activations and accepts a new one after the first ${outcome}`, async () => {
        const { transport, held } = holdingSnapshots();
        const { calls } = renderCounting(
          `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`,
          transport,
        );
        const button = await screen.findByRole("button", {
          name: "Take snapshot",
        });
        await act(async () => {
          // Mouse activations, then keyboard activations: a click event is
          // what Enter and Space produce on a native button.
          fireEvent.click(button);
          fireEvent.click(button);
          fireEvent.keyDown(button, { key: "Enter" });
          fireEvent.click(button);
          fireEvent.keyUp(button, { key: " " });
          fireEvent.click(button);
        });
        await settle(100);
        await take();
        await settle(100);
        expect(calls.CreateSnapshot).toBe(1);
        expect(held).toHaveLength(1);

        await act(async () => {
          held[0]!.release(
            outcome === "fails"
              ? new ConnectError("the snapshot failed", Code.Unavailable)
              : undefined,
          );
        });
        await settle(100);
        expect(calls.CreateSnapshot).toBe(1);

        // The button accepts a new submission once the first one settled.
        await take();
        await settle(100);
        expect(calls.CreateSnapshot).toBe(2);
        await act(async () => {
          held[1]!.release();
        });
        await settle(100);
      });
    }
  });
});

describe("sandbox transition notices", () => {
  function expectToast(type: "info" | "success" | "error", message: string) {
    return waitFor(() => {
      expect(
        document.querySelector(`.dui-toast[data-type="${type}"]`)?.textContent,
      ).toContain(message);
    });
  }

  for (const {
    action,
    nextAction,
    sandboxId,
    initialState,
    finalState,
    completion,
  } of [
    {
      action: "Pause",
      nextAction: "Resume",
      sandboxId: MOCK_IDS.runningSandbox,
      initialState: "Running",
      finalState: "Paused",
      completion: "The sandbox is paused.",
    },
    {
      action: "Resume",
      nextAction: "Pause",
      sandboxId: MOCK_IDS.pausedSandbox,
      initialState: "Paused",
      finalState: "Running",
      completion: "The sandbox is running.",
    },
  ]) {
    const path = `/orgs/${MOCK_IDS.acme}/sandboxes/${sandboxId}`;
    const method = `${action}Sandbox`;
    const requested = `${action} requested. Check the sandbox status.`;

    test(`reports ${action} completion when the mutation confirms the transition`, async () => {
      renderConsole(path);
      fireEvent.click(await screen.findByRole("button", { name: action }));
      await expectToast("success", completion);
      expect(screen.queryByText(requested)).toBeNull();
      expect(screen.getByText(finalState)).toBeTruthy();
      expect(screen.getByRole("button", { name: nextAction })).toBeTruthy();
    });

    for (const outcome of [
      SandboxTransitionOutcome.Pending,
      SandboxTransitionOutcome.Missing,
    ]) {
      test(`reports ${action} as requested with a ${outcome} transition response`, async () => {
        const mock = mockSandboxTransition(createMockTransport(), outcome);
        const { transport, calls } = countingTransport(mock);
        renderConsole(path, { transport });

        fireEvent.click(await screen.findByRole("button", { name: action }));

        await expectToast("info", requested);
        expect(screen.queryByText(completion)).toBeNull();
        expect(calls.GetSandbox).toBe(2);
        expect(screen.getByText(initialState)).toBeTruthy();
        expect(screen.getByRole("button", { name: action })).toBeTruthy();
      });
    }

    test(`keeps the existing error when ${action} is rejected`, async () => {
      const mock = mockSandboxTransition(
        createMockTransport(),
        SandboxTransitionOutcome.Rejected,
      );
      const { transport, calls } = countingTransport(mock);
      renderConsole(path, { transport });

      const button = await screen.findByRole("button", { name: action });
      fireEvent.click(button);

      await expectToast(
        "error",
        "OwnSand is not available right now. Try again in a moment.",
      );
      expect(screen.queryByText(completion)).toBeNull();
      expect(screen.queryByText(requested)).toBeNull();
      expect(calls.GetSandbox).toBe(1);
      expect(button.hasAttribute("data-pending")).toBe(false);
    });

    test(`sends one ${action} through the mutation and refresh`, async () => {
      const mock = mockSandboxTransition(
        createMockTransport(),
        SandboxTransitionOutcome.Pending,
      );
      let holdRefresh = false;
      const { transport, calls, seen } = holdPagedTransport(
        [method, "GetSandbox"],
        (name) => name === method || holdRefresh,
        mock,
      );
      renderConsole(path, { transport });
      const button = await screen.findByRole("button", { name: action });

      fireEvent.click(button);
      fireEvent.click(button);
      await waitFor(() => expect(calls[method]).toHaveLength(1));
      expect(button.getAttribute("aria-busy")).toBe("true");

      holdRefresh = true;
      await act(async () => calls[method]![0]!.settle());
      await waitFor(() => expect(calls.GetSandbox).toHaveLength(1));
      expect(button.getAttribute("aria-busy")).toBe("true");
      expect(screen.queryByText(requested)).toBeNull();
      fireEvent.click(button);

      await act(async () => calls.GetSandbox![0]!.settle());
      await expectToast("info", requested);
      expect(button.hasAttribute("data-pending")).toBe(false);
      expect(seen[method]).toBe(1);
    });
  }
});

/**
 * A transport that holds the responses of chosen calls until the test
 * releases them, and counts the calls of every method. `hold` receives the
 * method name and its 1-based call number.
 */
function holdingTransport(hold: (method: string, call: number) => boolean) {
  const mock = createMockTransport();
  const calls: Record<string, number> = {};
  const held: Record<
    string,
    Array<{ release(error?: ConnectError): void }>
  > = {};
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      calls[method.name] = (calls[method.name] ?? 0) + 1;
      if (hold(method.name, calls[method.name]!)) {
        await new Promise<void>((resolve, reject) => {
          (held[method.name] ??= []).push({
            release: (error) => (error ? reject(error) : resolve()),
          });
        });
      }
      return mock.unary(method, ...rest);
    },
  };
  return { transport, calls, held };
}

describe("device limits", () => {
  test("saves an explicit zero with the strict RPC", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`);
    const disk = await screen.findByLabelText("Disk limit (GiB)");
    await settle();
    await act(async () => {
      fireEvent.input(disk, { target: { value: "0" } });
      fireEvent.blur(disk);
    });
    await settle();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
    });
    await settle(60);
    expect(
      (await screen.findAllByText("The device limits are saved.")).length,
    ).toBeGreaterThan(0);
    expect(
      (screen.getByLabelText("Disk limit (GiB)") as HTMLInputElement).value,
    ).toBe("0");
  });

  /**
   * A mock transport whose build box has the given limits and that records
   * the ReplaceDeviceLimits requests.
   */
  async function recordingTransport(limits: {
    vcpuCount: number;
    memoryMib: bigint;
    diskGib: bigint;
  }) {
    const mock = createMockTransport();
    await createClient(DeviceService, mock).replaceDeviceLimits({
      organizationId: MOCK_IDS.acme,
      deviceId: MOCK_IDS.buildBox,
      limits: { ...limits, vcpuOvercommitPercent: 150 },
    });
    const replaced: DeviceLimitsInput[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "ReplaceDeviceLimits") {
          replaced.push((rest[3] as { limits: DeviceLimitsInput }).limits);
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, replaced };
  }

  const commit = async (input: HTMLElement, value: string) => {
    await act(async () => {
      fireEvent.input(input, { target: { value } });
      fireEvent.blur(input);
    });
    await settle();
  };

  const save = async () => {
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
    });
    await settle(60);
  };

  test("changing only the vCPU limit keeps memory and disk above 2^53 exact", async () => {
    const { transport, replaced } = await recordingTransport({
      vcpuCount: 16,
      memoryMib: 9223372036854775807n,
      diskGib: 9007199254740993n,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    const vcpu = await screen.findByLabelText("vCPU limit");
    await settle();
    expect(
      (screen.getByLabelText("Memory limit (MiB)") as HTMLInputElement).value,
    ).toBe("9223372036854775807");
    expect(
      (screen.getByLabelText("Disk limit (GiB)") as HTMLInputElement).value,
    ).toBe("9007199254740993");
    await commit(vcpu, "8");
    await save();
    expect(replaced).toHaveLength(1);
    expect(replaced[0]?.vcpuCount).toBe(8);
    expect(replaced[0]?.memoryMib).toBe(9223372036854775807n);
    expect(replaced[0]?.diskGib).toBe(9007199254740993n);
    expect(replaced[0]?.vcpuOvercommitPercent).toBe(150);
  });

  test("an explicitly edited limit keeps its exact precision", async () => {
    const { transport, replaced } = await recordingTransport({
      vcpuCount: 16,
      memoryMib: 65536n,
      diskGib: 500n,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    const disk = await screen.findByLabelText("Disk limit (GiB)");
    await settle();
    await commit(disk, "9007199254740993");
    await save();
    expect(replaced[0]?.diskGib).toBe(9007199254740993n);
    expect(replaced[0]?.memoryMib).toBe(65536n);
  });

  test("keeps an off-grid memory limit and sends 1000 as typed", async () => {
    const { transport, replaced } = await recordingTransport({
      vcpuCount: 16,
      memoryMib: 1000n,
      diskGib: 500n,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    const disk = await screen.findByLabelText("Disk limit (GiB)");
    await settle();
    // Saving a different limit keeps the off-grid memory value.
    await commit(disk, "600");
    await save();
    expect(replaced[0]?.memoryMib).toBe(1000n);
    const memory = screen.getByLabelText("Memory limit (MiB)");
    await commit(memory, "1500");
    await commit(memory, "1000");
    expect((memory as HTMLInputElement).value).toBe("1000");
    await save();
    expect(replaced[1]?.memoryMib).toBe(1000n);
    expect(replaced[1]?.diskGib).toBe(600n);
  });

  test("shows a validation error for an out-of-range limit and sends nothing", async () => {
    const { transport, replaced } = await recordingTransport({
      vcpuCount: 16,
      memoryMib: 65536n,
      diskGib: 500n,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    const memory = await screen.findByLabelText("Memory limit (MiB)");
    await settle();
    const message =
      "Memory limit must be a whole number from 0 to 9223372036854775807.";
    for (const value of ["9223372036854775808", "-1", "1.5"]) {
      await commit(memory, value);
      await save();
      expect(memory.getAttribute("aria-invalid")).toBe("true");
      expect(screen.getAllByText(message).length).toBeGreaterThan(0);
    }
    expect(replaced).toHaveLength(0);
    // Editing the field clears its error; the maximum is a valid value.
    await commit(memory, "9223372036854775807");
    expect(memory.getAttribute("aria-invalid")).not.toBe("true");
    await save();
    expect(replaced[0]?.memoryMib).toBe(9223372036854775807n);
  });

  test("a blank limit asks for the value and zero is a value", async () => {
    const { transport, replaced } = await recordingTransport({
      vcpuCount: 16,
      memoryMib: 65536n,
      diskGib: 500n,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    const memory = await screen.findByLabelText("Memory limit (MiB)");
    await settle();
    await commit(memory, "");
    await save();
    expect(
      screen.getAllByText("Memory limit is required. Enter 0 to allow none.")
        .length,
    ).toBeGreaterThan(0);
    expect(replaced).toHaveLength(0);
    await commit(memory, "0");
    await save();
    expect(replaced[0]?.memoryMib).toBe(0n);
    expect(replaced[0]?.vcpuCount).toBe(16);
    expect(replaced[0]?.diskGib).toBe(500n);
  });

  describe("while a save is pending", () => {
    const click = async () => {
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
      });
    };
    const LIMIT_FIELDS = [
      "vCPU limit",
      "Memory limit (MiB)",
      "Disk limit (GiB)",
    ];

    for (const outcome of ["succeeds", "fails"] as const) {
      test(`sends one request for repeated submissions and accepts a new one after the first ${outcome}`, async () => {
        const { transport, calls, held } = holdingTransport(
          (method) => method === "ReplaceDeviceLimits",
        );
        renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
          transport,
        });
        const disk = await screen.findByLabelText("Disk limit (GiB)");
        await settle();
        await commit(disk, "123");
        const button = screen.getByRole("button", { name: "Save limits" });
        await act(async () => {
          // Two submissions in the same tick, before React renders the
          // disabled button, then more activations while it is disabled.
          fireEvent.click(button);
          fireEvent.click(button);
          fireEvent.submit(button.closest("form")!);
        });
        await click();
        await settle(60);
        expect(calls.ReplaceDeviceLimits).toBe(1);
        expect(held.ReplaceDeviceLimits).toHaveLength(1);
        expect(
          (
            screen.getByRole("button", {
              name: "Save limits",
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(true);
        for (const label of LIMIT_FIELDS) {
          expect(
            (screen.getByLabelText(label) as HTMLInputElement).disabled,
          ).toBe(true);
        }

        await act(async () => {
          held.ReplaceDeviceLimits![0]!.release(
            outcome === "fails"
              ? new ConnectError("the save failed", Code.Unavailable)
              : undefined,
          );
        });
        await settle(100);
        expect(calls.ReplaceDeviceLimits).toBe(1);
        // The form is usable again and shows the draft (a failure) or the
        // saved value (a success).
        for (const label of LIMIT_FIELDS) {
          expect(
            (screen.getByLabelText(label) as HTMLInputElement).disabled,
          ).toBe(false);
        }
        expect(
          (screen.getByLabelText("Disk limit (GiB)") as HTMLInputElement).value,
        ).toBe("123");
        await click();
        await settle(60);
        expect(calls.ReplaceDeviceLimits).toBe(2);
        await act(async () => {
          held.ReplaceDeviceLimits![1]!.release();
        });
        await settle(100);
      });
    }

    test("keeps the form disabled until the refetch after the save has settled", async () => {
      // The first GetDevice call loads the page; the next one is the refetch
      // that follows the save.
      const { transport, calls, held } = holdingTransport(
        (method, call) => method === "GetDevice" && call > 1,
      );
      renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
        transport,
      });
      const disk = await screen.findByLabelText("Disk limit (GiB)");
      await settle();
      await commit(disk, "123");
      await click();
      await settle(100);
      expect(calls.ReplaceDeviceLimits).toBe(1);
      expect(held.GetDevice).toHaveLength(1);
      // The save is done and its readback is still running: the form stays
      // disabled, so no edit can be made that the readback would overwrite.
      const button = screen.getByRole("button", {
        name: "Save limits",
      }) as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect((disk as HTMLInputElement).disabled).toBe(true);
      expect((disk as HTMLInputElement).value).toBe("123");
      await click();
      await settle(60);
      expect(calls.ReplaceDeviceLimits).toBe(1);

      await act(async () => {
        held.GetDevice![0]!.release();
      });
      await settle(100);
      expect(button.disabled).toBe(false);
      expect((disk as HTMLInputElement).disabled).toBe(false);
      expect((disk as HTMLInputElement).value).toBe("123");
    });
  });
});

describe("agent version policy while a save is pending", () => {
  const SETTINGS = `/orgs/${MOCK_IDS.acme}/settings`;
  const save = async () => {
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    });
  };
  const pin = async (version: string) => {
    await act(async () => {
      fireEvent.click(screen.getByText("Pin a version"));
    });
    const field = await screen.findByLabelText("Pinned version");
    await act(async () => {
      fireEvent.input(field, { target: { value: version } });
      fireEvent.blur(field);
    });
    return field as HTMLInputElement;
  };

  for (const outcome of ["succeeds", "fails"] as const) {
    test(`sends one request for repeated submissions and accepts a new one after the first ${outcome}`, async () => {
      const { transport, calls, held } = holdingTransport(
        (method) => method === "UpdateAgentVersionPolicy",
      );
      renderConsole(SETTINGS, { transport });
      await screen.findByRole("heading", { name: "Agent version policy" });
      await settle(60);
      const field = await pin("1.4.2");
      const button = screen.getByRole("button", { name: "Save policy" });
      await act(async () => {
        // Two submissions in the same tick, before React renders the
        // disabled button, then another one while it is disabled.
        fireEvent.click(button);
        fireEvent.click(button);
        fireEvent.submit(button.closest("form")!);
      });
      await save();
      await settle(60);
      expect(calls.UpdateAgentVersionPolicy).toBe(1);
      expect((button as HTMLButtonElement).disabled).toBe(true);
      expect(field.disabled).toBe(true);

      await act(async () => {
        held.UpdateAgentVersionPolicy![0]!.release(
          outcome === "fails"
            ? new ConnectError("the save failed", Code.Unavailable)
            : undefined,
        );
      });
      await settle(100);
      expect(calls.UpdateAgentVersionPolicy).toBe(1);
      // The form is usable again and keeps the version in both outcomes: the
      // draft after a failure, the saved policy after a success.
      expect((button as HTMLButtonElement).disabled).toBe(false);
      expect(field.disabled).toBe(false);
      expect(field.value).toBe("1.4.2");
      await save();
      await settle(60);
      expect(calls.UpdateAgentVersionPolicy).toBe(2);
      await act(async () => {
        held.UpdateAgentVersionPolicy![1]!.release();
      });
      await settle(100);
    });
  }

  test("keeps the form disabled until the refetch after the save has settled", async () => {
    // The first call loads the page; the next one is the refetch that
    // follows the save.
    const { transport, calls, held } = holdingTransport(
      (method, call) => method === "GetAgentVersionPolicy" && call > 1,
    );
    renderConsole(SETTINGS, { transport });
    await screen.findByRole("heading", { name: "Agent version policy" });
    await settle(60);
    const field = await pin("1.4.2");
    await save();
    await settle(100);
    expect(calls.UpdateAgentVersionPolicy).toBe(1);
    expect(held.GetAgentVersionPolicy).toHaveLength(1);
    const button = screen.getByRole("button", {
      name: "Save policy",
    }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(field.disabled).toBe(true);
    expect(field.value).toBe("1.4.2");
    await save();
    await settle(60);
    expect(calls.UpdateAgentVersionPolicy).toBe(1);

    await act(async () => {
      held.GetAgentVersionPolicy![0]!.release();
    });
    await settle(100);
    expect(button.disabled).toBe(false);
    expect(field.disabled).toBe(false);
    expect(field.value).toBe("1.4.2");
  });
});

describe("sandbox create form resources", () => {
  /** A mock transport that records the resources of every CreateSandbox. */
  function recordingTransport() {
    const mock = createMockTransport();
    const created: Resources[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "CreateSandbox") {
          created.push((rest[3] as { resources: Resources }).resources);
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, created };
  }

  async function openForm(transport: Transport) {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Create sandbox" }),
      );
    });
    await settle(60);
    return screen.findByRole("dialog", { name: "Create a sandbox" });
  }

  const commit = async (input: HTMLElement, value: string) => {
    await act(async () => {
      fireEvent.input(input, { target: { value } });
      fireEvent.blur(input);
    });
    await settle();
  };

  test("keeps a Memory of 1000 and sends it", async () => {
    const { transport, created } = recordingTransport();
    const dialog = await openForm(transport);
    const memory = within(dialog).getByLabelText("Memory (MiB)");
    await commit(memory, "1000");
    // The field formats its value with a thousands separator.
    expect((memory as HTMLInputElement).value).toBe("1,000");
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
    expect(created).toHaveLength(1);
    expect(created[0]?.memoryMib).toBe(1000n);
  });

  const create = async (dialog: HTMLElement) => {
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
  };

  test("rejects fractional memory below the control minimum or beyond number precision", async () => {
    const { transport, created } = recordingTransport();
    const dialog = await openForm(transport);
    const memory = within(dialog).getByLabelText(
      "Memory (MiB)",
    ) as HTMLInputElement;
    for (const value of ["255.5", "1000.00000000000000001"]) {
      await commit(memory, value);
      await create(dialog);
      expect(created).toHaveLength(0);
      expect(dialog.textContent, value).toContain(
        "Enter a whole number of MiB from 1 to 9007199254740991.",
      );
      expect(memory.value).toBe(value);
      expect(memory.getAttribute("aria-invalid")).toBe("true");
    }
    await commit(memory, "1000");
    await create(dialog);
    expect(created).toHaveLength(1);
    expect(created[0]?.memoryMib).toBe(1000n);
  });

  test("rejects a vCPU count above the int32 range without a request", async () => {
    const { transport, created } = recordingTransport();
    const dialog = await openForm(transport);
    const vcpu = within(dialog).getByLabelText("vCPUs");
    for (const value of ["2147483648", "3000000000"]) {
      await commit(vcpu, value);
      await create(dialog);
      expect(vcpu.getAttribute("aria-invalid")).toBe("true");
      expect(dialog.textContent).toContain(
        "Enter a whole number of vCPUs from 1 to 2147483647.",
      );
      expect(dialog.querySelector(".notice-error")).toBeNull();
      expect(created).toHaveLength(0);
    }
    expect(
      screen.queryByRole("dialog", { name: "Create a sandbox" }),
    ).not.toBeNull();
  });

  test("rejects memory above 2^53 - 1 without a request", async () => {
    const { transport, created } = recordingTransport();
    const dialog = await openForm(transport);
    await commit(
      within(dialog).getByLabelText("Memory (MiB)"),
      "9007199254740993",
    );
    await create(dialog);
    expect(dialog.textContent).toContain("MiB from 1 to 9007199254740991");
    expect(dialog.querySelector(".notice-error")).toBeNull();
    expect(created).toHaveLength(0);
  });

  test("rejects disk above the control-plane limit and sends the corrected value", async () => {
    const { transport, created } = recordingTransport();
    const dialog = await openForm(transport);
    const disk = within(dialog).getByLabelText("Disk (GiB)");
    await commit(disk, "8589934592");
    await create(dialog);
    expect(disk.getAttribute("aria-invalid")).toBe("true");
    expect(dialog.textContent).toContain(
      "Enter a whole number of GiB from 1 to 8589934591.",
    );
    expect(dialog.querySelector(".notice-error")).toBeNull();
    expect(created).toHaveLength(0);
    expect(screen.getByRole("dialog", { name: "Create a sandbox" })).toBe(
      dialog,
    );

    await commit(disk, "8589934591");
    await create(dialog);
    expect(created).toHaveLength(1);
    expect(created[0]?.diskGib).toBe(8589934591n);
  });

  test("creates with the largest vCPU count of the int32 range", async () => {
    const { transport, created } = recordingTransport();
    const dialog = await openForm(transport);
    await commit(within(dialog).getByLabelText("vCPUs"), "2147483647");
    await create(dialog);
    expect(created).toHaveLength(1);
    expect(created[0]?.vcpuCount).toBe(2147483647);
    expect(
      screen.queryByRole("dialog", { name: "Create a sandbox" }),
    ).toBeNull();
  });

  test("labels ready snapshots of the same period with distinct ids and their creation time", async () => {
    // Snapshots that the mock creates back to back have UUID v7 ids with the
    // same time prefix; the option label must still tell them apart.
    const transport = createMockTransport();
    const snapshots = createClient(SnapshotService, transport);
    const made = [];
    for (let i = 0; i < 2; i++) {
      const res = await snapshots.createSnapshot({
        organizationId: MOCK_IDS.acme,
        sandboxId: MOCK_IDS.runningSandbox,
      });
      made.push(res.snapshot!);
    }
    const dialog = await openForm(transport);
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("combobox", { name: "Source" }));
    });
    const options = (await screen.findAllByRole("option")).filter((o) =>
      (o.textContent ?? "").startsWith("Snapshot "),
    );
    const texts = options.map((o) => o.textContent);
    expect(options).toHaveLength(3);
    expect(new Set(texts).size).toBe(3);
    for (const snapshot of made) {
      expect(texts).toContain(
        `Snapshot ${shortIdText(snapshot.snapshotId)} · ${formatTime(snapshot.createTime)}`,
      );
    }
  });

  test("creates a sandbox from the chosen snapshot option", async () => {
    const mock = createMockTransport();
    const sources: unknown[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "CreateSandbox") {
          sources.push((rest[3] as { source: unknown }).source);
        }
        return mock.unary(method, ...rest);
      },
    };
    const dialog = await openForm(transport);
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("combobox", { name: "Source" }));
    });
    const option = (await screen.findAllByRole("option")).find((o) =>
      (o.textContent ?? "").startsWith("Snapshot "),
    )!;
    await act(async () => {
      fireEvent.click(option);
    });
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
    expect(sources).toHaveLength(1);
    expect(
      JSON.stringify(sources[0], (_, v) =>
        typeof v === "bigint" ? String(v) : v,
      ),
    ).toContain(MOCK_IDS.snapshot);
  });

  test("shows the names of the sources on every open and sends the raw values", async () => {
    const mock = createMockTransport();
    const sources: unknown[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "CreateSandbox") {
          sources.push((rest[3] as { source: unknown }).source);
        }
        return mock.unary(method, ...rest);
      },
    };
    const dialog = await openForm(transport);
    const trigger = within(dialog).getByRole("combobox", { name: "Source" });
    const expectNames = async () => {
      await act(async () => {
        fireEvent.click(trigger);
      });
      const texts = (await screen.findAllByRole("option")).map(
        (o) => o.textContent ?? "",
      );
      expect(texts).toContain("Linux base image");
      expect(texts).toContain("Template: node-24-toolchain");
      for (const text of texts) {
        expect(text).not.toMatch(/^(base|template|snapshot):/);
      }
      expect(trigger.textContent).toContain("Linux base image");
    };
    expect(trigger.textContent).toContain("Linux base image");
    await act(async () => {
      fireEvent.pointerEnter(trigger);
      fireEvent.mouseEnter(trigger);
    });
    expect(trigger.textContent).toContain("Linux base image");
    expect(trigger.textContent).not.toContain("base:");
    await expectNames();
    await act(async () => {
      fireEvent.keyDown(document.activeElement ?? trigger, { key: "Escape" });
    });
    await settle();
    await expectNames();

    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
    expect(sources).toHaveLength(1);
    expect(
      JSON.stringify(sources[0], (_, v) =>
        typeof v === "bigint" ? String(v) : v,
      ),
    ).toBe(
      JSON.stringify({ source: { case: "baseImage", value: BaseImage.LINUX } }),
    );
  });

  test("keeps the minimum of 256 and steps by whole numbers", async () => {
    const { transport } = recordingTransport();
    const dialog = await openForm(transport);
    const memory = within(dialog).getByLabelText(
      "Memory (MiB)",
    ) as HTMLInputElement;
    await commit(memory, "100");
    expect(memory.value).toBe("256");
    await commit(memory, "1000");
    await act(async () => {
      fireEvent.keyDown(memory, { key: "ArrowUp" });
    });
    await settle();
    expect(memory.value).toBe("1,001");
    await act(async () => {
      fireEvent.keyDown(memory, { key: "ArrowUp", shiftKey: true });
    });
    await settle();
    expect(memory.value).toBe("1,257");
    // The plus button of the Memory field adds one more whole number.
    const plus = memory
      .closest('[data-slot="number-input-group"]')!
      .querySelector('[data-slot="number-input-increment"]')!;
    await act(async () => {
      fireEvent.pointerDown(plus, { button: 0 });
      fireEvent.pointerUp(plus, { button: 0 });
      fireEvent.click(plus);
    });
    await settle();
    expect(memory.value).toMatch(/^1,25[89]$/);
  });
});

describe("invitations", () => {
  test("keeps the secret only for the sign-in redirect and removes it from the address", async () => {
    const { signIns } = renderConsole(
      `/invitations/accept#secret=${MOCK_INVITATION_SECRET}`,
      { signedIn: false },
    );
    expect(window.location.hash).toBe("");
    expect(pendingInvitation.read()).toBeUndefined();
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Sign in to accept" }),
      );
    });
    expect(signIns).toHaveLength(1);
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });

  test("shows an alert next to Sign in to accept when the sign-in did not start", async () => {
    renderConsole(`/invitations/accept#secret=${MOCK_INVITATION_SECRET}`, {
      signedIn: false,
      signInFailed: true,
    });
    expect(
      (
        await screen.findByText(
          "Sign-in did not start. Check your connection and try again.",
        )
      ).getAttribute("role"),
    ).toBe("alert");
    expect(
      (
        screen.getByRole("button", {
          name: "Sign in to accept",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  });

  test("shows the sign-in configuration notice instead of a sign-in button when Logto is not configured", async () => {
    // The default build: Logto mode without a registration, so no sign-in
    // can complete and the secret has no redirect to survive.
    const { signIns } = renderConsole(
      `/invitations/accept#secret=${MOCK_INVITATION_SECRET}`,
      {
        signedIn: false,
        config: {
          apiOrigin: "https://api.test",
          authMode: AuthMode.Logto,
          logto: undefined,
        },
      },
    );
    expect(
      await screen.findByText("Sign-in is not configured for this build."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in to accept" })).toBe(
      null,
    );
    expect(signIns).toHaveLength(0);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(window.sessionStorage.length).toBe(0);
    // The page still removes the secret from the address bar.
    expect(window.location.hash).toBe("");
  });

  test("accepts the pending invitation after sign-in and deletes the stored secret", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept");
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.contoso}/sandboxes`,
    );
  });

  test("deletes the stored secret and shows the error with the accept button when AcceptInvitation fails", async () => {
    // The mock user already belongs to Acme, so the control plane refuses it.
    pendingInvitation.save(MOCK_ACME_INVITATION_SECRET);
    renderConsole("/invitations/accept");
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(await screen.findByText("you are already a member")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(screen.queryByText(/needs an invitation link/)).toBeNull();
  });

  test("accepts again with the same secret after a transient failure", async () => {
    const mock = createMockTransport();
    let acceptCalls = 0;
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "AcceptInvitation") {
          acceptCalls += 1;
          if (acceptCalls === 1) {
            throw new ConnectError("down", Code.Unavailable);
          }
        }
        return mock.unary(method, ...rest);
      },
    };
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { transport });
    const click = async () => {
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("button", { name: "Accept invitation" }),
        );
      });
      await settle(60);
    };
    await click();
    expect(pendingInvitation.read()).toBeUndefined();
    expect(await screen.findByText(/not available right now/)).toBeTruthy();
    expect(window.location.pathname).toBe("/invitations/accept");
    await click();
    expect(acceptCalls).toBe(2);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.contoso}/sandboxes`,
    );
  });

  test("clears the previous error when a new invitation link opens", async () => {
    pendingInvitation.save(MOCK_ACME_INVITATION_SECRET);
    renderConsole("/invitations/accept");
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(await screen.findByText("you are already a member")).toBeTruthy();
    await act(async () => {
      window.location.hash = `secret=${MOCK_INVITATION_SECRET}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle(30);
    expect(screen.queryByText("you are already a member")).toBeNull();
    expect(
      screen.getByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
  });

  /** Holds AcceptInvitation and records each request with its mock response. */
  function delayedAcceptTransport() {
    const mock = createMockTransport();
    const calls: {
      invitationSecret: string;
      response: ReturnType<Transport["unary"]>;
    }[] = [];
    let release = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name !== "AcceptInvitation") {
          return mock.unary(method, ...rest);
        }
        const { invitationSecret } = rest[3] as AcceptInvitationRequest;
        const response = gate.then(() => mock.unary(method, ...rest));
        calls.push({ invitationSecret, response });
        return response;
      },
    };

    async function expectContosoRequest() {
      await waitFor(() => {
        expect(calls.map((call) => call.invitationSecret)).toEqual([
          MOCK_INVITATION_SECRET,
        ]);
      });
    }

    async function releaseAndExpectContosoResponse() {
      const timeout = new AbortController();
      const response = Promise.race([
        calls[0]!.response,
        delay(1_000, undefined, { signal: timeout.signal }).then(() => {
          throw new Error("Timed out waiting for AcceptInvitation");
        }),
      ]).finally(() => timeout.abort());
      await act(async () => {
        release();
        await Promise.allSettled([response]);
      });
      expect((await response).message).toMatchObject({
        organization: { organizationId: MOCK_IDS.contoso },
      });
    }

    return {
      transport,
      release,
      expectContosoRequest,
      releaseAndExpectContosoResponse,
    };
  }

  test("does not navigate when an earlier accept succeeds after a new link opened", async () => {
    const held = delayedAcceptTransport();
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { transport: held.transport });
    const earlierButton = await screen.findByRole("button", {
      name: "Accept invitation",
    });
    await act(async () => {
      fireEvent.click(earlierButton);
    });
    await held.expectContosoRequest();
    await act(async () => {
      window.location.hash = `secret=${MOCK_ACME_INVITATION_SECRET}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    const replacementButton = screen.getByRole("button", {
      name: "Accept invitation",
    });
    expect(replacementButton).not.toBe(earlierButton);
    await held.releaseAndExpectContosoResponse();
    expect(window.location.pathname).toBe("/invitations/accept");
    expect(screen.getByRole("button", { name: "Accept invitation" })).toBe(
      replacementButton,
    );
  });

  /** Counts ListOrganizations calls and records their page tokens. */
  function listOrganizationsProbe(mock = createMockTransport()) {
    const state = { calls: 0, tokens: [] as string[] };
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "ListOrganizations") {
          state.calls += 1;
          state.tokens.push((rest[3] as { pageToken: string }).pageToken);
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, state };
  }

  /** Pastes the Contoso invitation link on the organizations page and accepts it. */
  async function joinContosoFromOrganizations() {
    await screen.findByRole("link", { name: /Acme Robotics/ });
    fireEvent.input(screen.getByLabelText("Invitation link"), {
      target: {
        value: `https://console.test/invitations/accept#secret=${MOCK_INVITATION_SECRET}`,
      },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
  }

  test("lists the joined organization in the switcher while the organization list is still fresh", async () => {
    const { transport, state } = listOrganizationsProbe();
    // The production client: ListOrganizations stays fresh for 10 seconds.
    renderConsole("/", { transport, queryClient: createQueryClient() });
    await joinContosoFromOrganizations();
    await settle(100);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.contoso}/sandboxes`,
    );
    // The page and the switcher read the list once. Accepting refetches it.
    expect(state.calls).toBeGreaterThan(1);
    const trigger = await screen.findByRole("combobox", {
      name: "Organization",
    });
    expect(trigger.textContent).toContain("Contoso Research");
  });

  test("lists organizations past the first page of the mock and refreshes them after a membership change", async () => {
    const mock = createMockTransport();
    const client = createClient(OrganizationService, mock);
    // 2 seeded organizations and 200 more fill the first page and leave a
    // second one.
    for (let i = 0; i < 200; i++) {
      await client.createOrganization({ name: `Extra ${i}` });
    }
    const { transport, state } = listOrganizationsProbe(mock);
    renderConsole("/", { transport, queryClient: createQueryClient() });
    expect(await screen.findByRole("link", { name: /Extra 199/ })).toBeTruthy();
    expect(state.tokens).toHaveLength(2);
    expect(state.tokens[0]).toBe("");
    expect(state.tokens[1]).not.toBe("");
    // Joining Contoso refreshes the shared list from the first page.
    await joinContosoFromOrganizations();
    await settle(100);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.contoso}/sandboxes`,
    );
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    expect(
      await screen.findByRole("option", { name: "Contoso Research" }),
    ).toBeTruthy();
    expect(screen.getByRole("option", { name: "Extra 199" })).toBeTruthy();
    expect(state.tokens.length).toBeGreaterThan(2);
  });

  test("refreshes the organizations when an earlier accept succeeds after the user left the page", async () => {
    const { transport, release } = delayedAcceptTransport();
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", {
      transport,
      queryClient: createQueryClient(),
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("link", { name: "Not now" }));
    });
    expect(window.location.pathname).toBe("/");
    await screen.findByRole("link", { name: /Acme Robotics/ });
    expect(screen.queryByRole("link", { name: /Contoso Research/ })).toBeNull();
    release();
    await settle(100);
    // The late success does not navigate, and the list shows the new
    // organization although it was fetched less than 10 seconds before.
    expect(window.location.pathname).toBe("/");
    expect(
      await screen.findByRole("link", { name: /Contoso Research/ }),
    ).toBeTruthy();
  });

  test("does not navigate when an earlier accept succeeds after the user left the page", async () => {
    const held = delayedAcceptTransport();
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { transport: held.transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await held.expectContosoRequest();
    await act(async () => {
      fireEvent.click(screen.getByRole("link", { name: "Not now" }));
    });
    expect(window.location.pathname).toBe("/");
    expect(screen.getByRole("heading", { name: "Organizations" })).toBeTruthy();
    await held.releaseAndExpectContosoResponse();
    expect(window.location.pathname).toBe("/");
    expect(screen.getByRole("heading", { name: "Organizations" })).toBeTruthy();
  });

  test("a new link drops the stored secret of an earlier link", async () => {
    pendingInvitation.save(MOCK_ACME_INVITATION_SECRET);
    renderConsole(`/invitations/accept#secret=${MOCK_INVITATION_SECRET}`);
    expect(
      await screen.findByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
    expect(window.location.hash).toBe("");
    expect(pendingInvitation.read()).toBeUndefined();
    cleanup();
    renderConsole("/invitations/accept");
    expect(await screen.findByText(/needs an invitation link/)).toBeTruthy();
  });

  test("accepts the new link and not the stored secret of an earlier link", async () => {
    // The stored secret would fail with "you are already a member".
    pendingInvitation.save(MOCK_ACME_INVITATION_SECRET);
    renderConsole(`/invitations/accept#secret=${MOCK_INVITATION_SECRET}`);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.contoso}/sandboxes`,
    );
  });

  test("a link opened while the page is shown drops the stored secret of an earlier link", async () => {
    pendingInvitation.save(MOCK_ACME_INVITATION_SECRET);
    renderConsole("/invitations/accept");
    expect(
      await screen.findByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
    await act(async () => {
      window.location.hash = `secret=${MOCK_INVITATION_SECRET}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle(30);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(window.location.hash).toBe("");
    cleanup();
    renderConsole("/invitations/accept");
    expect(await screen.findByText(/needs an invitation link/)).toBeTruthy();
  });

  test("keeps the stored secret for the sign-in return when the fragment is empty", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { signedIn: false });
    expect(
      await screen.findByRole("button", { name: "Sign in to accept" }),
    ).toBeTruthy();
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });

  test("shows the missing-secret screen when the page opens without a secret", async () => {
    renderConsole("/invitations/accept");
    expect(await screen.findByText(/needs an invitation link/)).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Accept invitation" }),
    ).toBeNull();
  });
});

describe("a page load outside the invitation flow", () => {
  test.each([
    ["the organization list", "/"],
    ["an organization page", `/orgs/${MOCK_IDS.acme}/sandboxes`],
    ["an unknown route", "/no-such-page"],
  ])("deletes the stored secret on a fresh load of %s", async (_name, path) => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole(path);
    await settle(60);
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("deletes the stored secret on a fresh load while signed out", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/", { signedIn: false });
    await settle(60);
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("keeps the stored secret on a reload of the signed-out invitation page", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { signedIn: false, strict: true });
    expect(
      await screen.findByRole("button", { name: "Sign in to accept" }),
    ).toBeTruthy();
    await settle(60);
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });

  test("keeps the stored secret on a load of the sign-in callback and returns to the invitation", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/callback", { strict: true });
    expect(
      await screen.findByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
    expect(window.location.pathname).toBe("/invitations/accept");
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });

  test("a sign-in after an abandoned invitation returns to the organization list", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    // The user abandoned the sign-in and loaded the console again ...
    renderConsole("/");
    await settle(60);
    cleanup();
    // ... and later signs in, which returns to the callback route.
    renderConsole("/callback", { strict: true });
    expect(
      await screen.findByRole("link", { name: /Acme Robotics/ }),
    ).toBeTruthy();
    expect(window.location.pathname).toBe("/");
  });
});

describe("history navigation out of the invitation flow", () => {
  /** jsdom fires no popstate for pushState, so the test dispatches it. */
  async function historyNavigate(path: string) {
    await act(async () => {
      window.history.pushState(null, "", path);
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
  }

  test("deletes the stored secret when Back or Forward leaves the invitation flow", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept");
    await screen.findByRole("button", { name: "Accept invitation" });
    await historyNavigate("/");
    expect(pendingInvitation.read()).toBeUndefined();
    expect(
      await screen.findByRole("link", { name: /Acme Robotics/ }),
    ).toBeTruthy();
  });

  test("keeps the stored secret when history navigation moves to the sign-in callback", async () => {
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept");
    await screen.findByRole("button", { name: "Accept invitation" });
    await historyNavigate("/callback");
    await settle(60);
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });

  test("runs the navigation listeners once per history navigation and once per link navigation", async () => {
    const calls: string[] = [];
    window.history.replaceState(null, "", "/");
    render(
      <RouterProvider
        onNavigate={(from, to) => calls.push(`${from.id}>${to.id}`)}
      >
        <Probe />
      </RouterProvider>,
    );
    await historyNavigate("/invitations/accept");
    await historyNavigate("/callback");
    expect(calls).toEqual([
      `${RouteId.Organizations}>${RouteId.Invitation}`,
      `${RouteId.Invitation}>${RouteId.Callback}`,
    ]);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "go" }));
    });
    expect(calls).toHaveLength(3);
    expect(calls[2]).toBe(`${RouteId.Callback}>${RouteId.Organizations}`);
  });
});

function Probe() {
  const { navigate } = useRouter();
  return (
    <button type="button" onClick={() => navigate("/")}>
      go
    </button>
  );
}

describe("sign-in callback", () => {
  test("shows the sign-in configuration notice when Logto is not configured", async () => {
    // The default build: Logto mode without a registration, so no Logto
    // provider exists and the Logto callback hook has nothing to complete.
    renderConsole("/callback", {
      signedIn: false,
      config: {
        apiOrigin: "https://api.test",
        authMode: AuthMode.Logto,
        logto: undefined,
      },
    });
    // The toast region has its own alert announcer, so match the text.
    expect(
      await screen.findByText("Sign-in is not configured for this build."),
    ).toBeTruthy();
  });

  test("the mock callback still returns to the organizations", async () => {
    renderConsole("/callback", { signedIn: false });
    await settle(60);
    expect(window.location.pathname).toBe("/");
  });
});

/** The seeded Acme members of each role. */
const ACME_ROLES = [
  ["owner", MOCK_USER],
  ["admin", "user-admin-jun"],
  ["member", "user-member-ada"],
] as const;

const BILLING_STATES = [
  MockBillingState.AllowedLinked,
  MockBillingState.BlockedUnlinked,
  MockBillingState.BlockedLinked,
  MockBillingState.Suspended,
] as const;

/** A mock transport that counts the calls of each method. */
function countingTransport(mock: Transport) {
  const calls: Record<string, number> = {};
  const transport: Transport = {
    ...mock,
    unary(method, ...rest) {
      calls[method.name] = (calls[method.name] ?? 0) + 1;
      return mock.unary(method, ...rest);
    },
  };
  return { transport, calls };
}

describe("billing banner", () => {
  for (const [roleName, userId] of ACME_ROLES) {
    for (const state of BILLING_STATES) {
      test(`the ${roleName} in ${state}`, async () => {
        renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
          transport: createMockTransport({
            userId,
            billing: { [MOCK_IDS.acme]: state },
          }),
        });
        await screen.findByRole("heading", { name: "Sandboxes", level: 1 });
        await settle(60);
        const banner = screen.queryByRole("region", {
          name: /This organization is/,
        });
        if (state === MockBillingState.AllowedLinked) {
          expect(banner).toBeNull();
          return;
        }
        expect(banner).not.toBeNull();
        const text = banner!.textContent!;
        const link = within(banner!).getByRole("link", {
          name: "Manage billing at accounts.delino.io",
        });
        expect(link.getAttribute("href")).toBe("https://accounts.delino.io");
        if (state === MockBillingState.Suspended) {
          expect(text).toContain("This organization is suspended");
          expect(text).toContain("Running sandboxes are paused");
          expect(text).toContain("Suspended since");
        } else {
          expect(text).toContain("This organization is blocked");
          expect(text).toContain("OwnSand rejects creating sandboxes");
          expect(text).toContain("is suspended on");
        }
        if (state === MockBillingState.BlockedUnlinked) {
          expect(text).toContain("No billing account is linked");
          expect(
            within(banner!).queryByRole("link", {
              name: "Link a billing account",
            }) !== null,
          ).toBe(roleName === "owner");
          expect(text.includes("The organization owner must link")).toBe(
            roleName !== "owner",
          );
        }
        if (state === MockBillingState.BlockedLinked) {
          expect(text).toContain("Payment is missing");
        }
      });
    }
  }

  // Every organization route, with the level-one heading that proves the
  // requested page (not a loading or error state) rendered.
  const ORGANIZATION_ROUTES = [
    { name: "sandboxes", path: "sandboxes", heading: "Sandboxes" },
    { name: "devices", path: "devices", heading: "Devices" },
    { name: "members", path: "members", heading: "Members" },
    {
      name: "enrollment tokens",
      path: "enrollment-tokens",
      heading: "Enrollment tokens",
    },
    { name: "API keys", path: "api-keys", heading: "API keys" },
    { name: "snapshots", path: "snapshots", heading: "Snapshots" },
    { name: "settings", path: "settings", heading: "Settings" },
    {
      name: "the seeded sandbox",
      path: `sandboxes/${MOCK_IDS.runningSandbox}`,
      heading: "Sandbox",
    },
    {
      name: "the seeded device",
      path: `devices/${MOCK_IDS.buildBox}`,
      heading: /build-box/,
    },
  ] as const;

  for (const route of ORGANIZATION_ROUTES) {
    test(`shows on ${route.name}`, async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/${route.path}`, {
        transport: createMockTransport({
          userId: MOCK_USER,
          billing: { [MOCK_IDS.acme]: MockBillingState.BlockedLinked },
        }),
      });
      expect(
        await screen.findByRole("heading", { name: route.heading, level: 1 }),
      ).toBeTruthy();
      expect(
        await screen.findByRole("region", {
          name: "This organization is blocked",
        }),
      ).toBeTruthy();
    });
  }
});

describe("billing errors", () => {
  test("a mutation rejected by billing shows the message and link once and refreshes the status", async () => {
    const { transport, calls } = countingTransport(
      createMockTransport({
        billing: { [MOCK_IDS.acme]: MockBillingState.BlockedLinked },
      }),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    await screen.findByRole("region", { name: "This organization is blocked" });
    await screen.findByText(/ose_91c2/);
    const statusReads = calls.GetBillingStatus ?? 0;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));
    });
    const dialog = await screen.findByRole("dialog");
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Create token" }),
      );
    });
    await settle(100);
    const notice = dialog.querySelector(".notice-error") as HTMLElement;
    expect(notice.textContent).toContain(
      "has no active subscription or no balance",
    );
    expect(
      within(notice)
        .getByRole("link", { name: "https://accounts.delino.io" })
        .getAttribute("href"),
    ).toBe("https://accounts.delino.io");
    expect(within(notice).queryByRole("button", { name: "Try again" })).toBe(
      null,
    );
    // Never retried, and the banner's status is read again.
    expect(calls.CreateEnrollmentToken).toBe(1);
    expect(calls.GetBillingStatus ?? 0).toBeGreaterThan(statusReads);
  });
});

describe("billing settings", () => {
  for (const [roleName, userId] of ACME_ROLES) {
    for (const state of BILLING_STATES) {
      test(`the ${roleName} in ${state}`, async () => {
        const { transport, calls } = countingTransport(
          createMockTransport({ userId, billing: { [MOCK_IDS.acme]: state } }),
        );
        renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
        await screen.findByRole("heading", { name: "Agent version policy" });
        await settle(60);
        const section = screen.queryByRole("region", { name: "Billing" });
        if (roleName === "member") {
          expect(section).toBeNull();
          expect(calls.GetBillingUsage).toBeUndefined();
          return;
        }
        expect(section).not.toBeNull();
        const text = section!.textContent!;
        const owner = roleName === "owner";
        if (state === MockBillingState.BlockedUnlinked) {
          // Without a link, the section never asks for the usage.
          expect(calls.GetBillingUsage).toBeUndefined();
          expect(
            within(section!).queryByRole("button", {
              name: "Link a billing account",
            }) !== null,
          ).toBe(owner);
          expect(text.includes("The organization owner must link one")).toBe(
            !owner,
          );
          return;
        }
        await within(section!).findByRole("table", {
          name: "Usage this month",
        });
        const rows = within(section!)
          .getAllByRole("row")
          .slice(1)
          .map((r) => r.textContent);
        expect(rows).toEqual([
          "Relay egress12.50 GiB$1.13",
          "R2 snapshot storage0.986 GiB-months$0.03",
          "Class A RPCs1,240 calls$0.00558",
          "Class B RPCs18,400 calls$0.006624",
        ]);
        expect(text).toContain("$1.17");
        if (state === MockBillingState.AllowedLinked) {
          expect(text).toContain("Acme Robotics billing");
          expect(text).toContain("$42.52");
        } else {
          expect(text).toContain("Research lab credits");
          expect(text).toContain("-$1.23 (debt)");
        }
        expect(
          within(section!).getByRole("link", { name: "accounts.delino.io" }),
        ).toBeTruthy();
        expect(
          within(section!).queryByRole("button", {
            name: "Unlink billing account",
          }) !== null,
        ).toBe(owner);
      });
    }
  }

  async function openLinkDialog() {
    const section = await screen.findByRole("region", { name: "Billing" });
    // Find outside act: a findBy wait inside act logs act warnings.
    const open = await within(section).findByRole("button", {
      name: "Link a billing account",
    });
    await act(async () => {
      fireEvent.click(open);
    });
    await settle(60);
    return screen.findByRole("dialog", { name: "Link a billing account" });
  }

  function renderBillingLink() {
    const queryClient = createQueryClient();
    const options = {
      billing: { [MOCK_IDS.acme]: MockBillingState.BlockedUnlinked },
    };
    const mock = createMockTransport(options);
    const withoutAcme = createMockTransport({
      ...options,
      linkableBillingAccounts: MOCK_BILLING_ACCOUNTS.slice(1),
    });
    const listMethod = BillingService.method.listLinkableBillingAccounts.name;
    const links: string[] = [];
    let hideAcme = false;
    const { transport, calls } = holdPagedTransport([listMethod], undefined, {
      ...mock,
      unary(method, ...rest) {
        if (method.name === BillingService.method.linkBillingAccount.name) {
          links.push(
            (rest[3] as unknown as LinkBillingAccountRequest).billingAccountId,
          );
        }
        const source =
          method.name === listMethod && hideAcme ? withoutAcme : mock;
        return source.unary(method, ...rest);
      },
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      queryClient,
      transport,
    });
    return {
      links,
      removeAcme: () => {
        hideAcme = true;
      },
      refetch: () => {
        void queryClient.refetchQueries({
          type: "active",
          queryKey: createConnectQueryKey({
            schema: BillingService.method.listLinkableBillingAccounts,
            cardinality: undefined,
          }),
        });
      },
      async finishListRequest(index: number, error?: Error) {
        await waitFor(() => expect(calls[listMethod]?.[index]).toBeDefined());
        await act(async () => calls[listMethod]![index]!.settle(error));
      },
    };
  }

  async function submitLinkForm(dialog: HTMLElement) {
    const button = within(dialog).getByRole<HTMLButtonElement>("button", {
      name: "Link",
    });
    await act(async () => {
      fireEvent.click(button);
      fireEvent.submit(button.form!);
    });
  }

  test.each([
    [
      Code.PermissionDenied,
      "You do not have permission to do this.",
      "Try again",
    ],
    [
      Code.NotFound,
      "It was not found, or you no longer have access to it.",
      null,
    ],
    [
      Code.Unauthenticated,
      "Your session ended. Sign in again.",
      "Sign in with Delino",
    ],
  ] as const)(
    "blocks a billing link after terminal error %s",
    async (code, message, action) => {
      const fixture = renderBillingLink();
      const dialog = await openLinkDialog();
      const body = within(dialog);
      await fixture.finishListRequest(0);
      const account = await body.findByRole("radio", {
        name: /Acme Robotics billing/,
      });
      await act(async () => fireEvent.click(account));
      expect(account.getAttribute("aria-checked")).toBe("true");

      fixture.refetch();
      await fixture.finishListRequest(
        1,
        new ConnectError("List refresh failed", code),
      );
      expect((await body.findByRole("alert")).textContent).toContain(message);
      expect(body.queryAllByRole("radio")).toHaveLength(0);
      expect(
        body.getByRole<HTMLButtonElement>("button", { name: "Link" }).disabled,
      ).toBe(true);
      if (action === null) {
        expect(body.queryByRole("button", { name: "Try again" })).toBeNull();
      } else {
        expect(body.getByRole("button", { name: action })).toBeTruthy();
      }
      await submitLinkForm(dialog);
      expect(fixture.links).toEqual([]);
    },
  );

  test.each([true, false])(
    "recovers from a billing list error (original account available: %s)",
    async (keepAcme) => {
      const fixture = renderBillingLink();
      const dialog = await openLinkDialog();
      const body = within(dialog);
      await fixture.finishListRequest(0);
      const account = await body.findByRole("radio", {
        name: /Acme Robotics billing/,
      });
      await act(async () => fireEvent.click(account));
      fixture.refetch();
      await fixture.finishListRequest(
        1,
        new ConnectError("List refresh failed", Code.PermissionDenied),
      );
      const retry = await body.findByRole("button", { name: "Try again" });

      if (!keepAcme) fixture.removeAcme();
      await act(async () => fireEvent.click(retry));
      await fixture.finishListRequest(2);
      const lab = await body.findByRole("radio", {
        name: /Research lab credits/,
      });
      expect(body.queryByRole("alert")).toBeNull();
      const acme = body.queryByRole("radio", { name: /Acme Robotics billing/ });
      if (keepAcme) {
        expect(acme?.getAttribute("aria-checked")).toBe("true");
      } else {
        expect(acme).toBeNull();
        expect(lab.getAttribute("aria-checked")).toBe("false");
        await act(async () => fireEvent.submit(lab.closest("form")!));
        expect(await body.findByText("Choose a billing account.")).toBeTruthy();
        expect(fixture.links).toEqual([]);
        await act(async () => fireEvent.click(lab));
      }
      await act(async () =>
        fireEvent.click(body.getByRole("button", { name: "Link" })),
      );
      expect(fixture.links).toEqual([
        keepAcme ? MOCK_IDS.acmeBillingAccount : MOCK_IDS.labBillingAccount,
      ]);
    },
  );

  test("links the visible selection after a transient billing list error", async () => {
    const fixture = renderBillingLink();
    const body = within(await openLinkDialog());
    await fixture.finishListRequest(0);
    const account = await body.findByRole("radio", {
      name: /Acme Robotics billing/,
    });
    await act(async () => fireEvent.click(account));
    fixture.refetch();
    await fixture.finishListRequest(
      1,
      new ConnectError("List refresh failed", Code.Internal),
    );

    expect((await body.findByRole("alert")).textContent).toContain(
      "The last refresh failed",
    );
    expect(account.getAttribute("aria-checked")).toBe("true");
    await act(async () =>
      fireEvent.click(body.getByRole("button", { name: "Link" })),
    );
    expect(fixture.links).toEqual([MOCK_IDS.acmeBillingAccount]);
  });

  test("blocks a billing link during loading and on the first list failure", async () => {
    const fixture = renderBillingLink();
    const dialog = await openLinkDialog();
    const body = within(dialog);
    expect(
      body.getByRole<HTMLButtonElement>("button", { name: "Link" }).disabled,
    ).toBe(true);
    await submitLinkForm(dialog);

    await fixture.finishListRequest(
      0,
      new ConnectError("List failed", Code.Internal),
    );
    await body.findByRole("alert");
    expect(
      body.getByRole<HTMLButtonElement>("button", { name: "Link" }).disabled,
    ).toBe(true);
    await submitLinkForm(dialog);
    expect(fixture.links).toEqual([]);
  });

  test("the owner links a billing account and the block ends", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      transport: createMockTransport({
        billing: { [MOCK_IDS.acme]: MockBillingState.BlockedUnlinked },
      }),
    });
    await screen.findByRole("region", { name: "This organization is blocked" });
    const dialog = await openLinkDialog();
    const first = within(dialog).getByRole("radio", {
      name: /Acme Robotics billing · Owner/,
    });
    expect(document.activeElement).toBe(first);
    expect(
      within(dialog).getByRole("radio", {
        name: /Research lab credits · Admin/,
      }),
    ).toBeTruthy();
    await act(async () => {
      fireEvent.click(first);
    });
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Link" }));
    });
    await settle(100);
    expect(screen.queryByRole("dialog")).toBeNull();
    const section = screen.getByRole("region", { name: "Billing" });
    expect(
      (await within(section).findByText("Acme Robotics billing")).textContent,
    ).toBe("Acme Robotics billing");
    await settle(60);
    expect(
      screen.queryByRole("region", { name: /This organization is/ }),
    ).toBeNull();
  });

  test("Escape closes the link dialog", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      transport: createMockTransport({
        billing: { [MOCK_IDS.acme]: MockBillingState.BlockedUnlinked },
      }),
    });
    const dialog = await openLinkDialog();
    await within(dialog).findAllByRole("radio");
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("explains where to create a billing account when none can be linked", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      transport: createMockTransport({
        billing: { [MOCK_IDS.acme]: MockBillingState.BlockedUnlinked },
        linkableBillingAccounts: [],
      }),
    });
    const dialog = await openLinkDialog();
    await within(dialog).findByText(/not an owner or admin of any billing/);
    expect(
      within(dialog)
        .getByRole("link", { name: "accounts.delino.io" })
        .getAttribute("href"),
    ).toBe("https://accounts.delino.io");
    expect(
      (
        within(dialog).getByRole("button", {
          name: "Link",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
  });

  test("the owner unlinks after a confirmation and the organization is blocked at once", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
    const section = await screen.findByRole("region", { name: "Billing" });
    const unlink = await within(section).findByRole("button", {
      name: "Unlink billing account",
    });
    await act(async () => {
      fireEvent.click(unlink);
    });
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog.textContent).toContain("becomes blocked at once");
    const buttons = within(dialog).getAllByRole("button");
    expect(buttons[0].textContent).toBe("Cancel");
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Unlink billing account" }),
      );
    });
    await settle(100);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(
      await screen.findByRole("region", {
        name: "This organization is blocked",
      }),
    ).toBeTruthy();
    expect(
      await within(section).findByRole("button", {
        name: "Link a billing account",
      }),
    ).toBeTruthy();
  });

  test("the ownership transfer confirmation says that it removes the billing link", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
    await screen.findByRole("region", { name: "Billing" });
    await settle(60);
    await act(async () => {
      fireEvent.click(screen.getByRole("combobox", { name: "New owner" }));
    });
    const option = await screen.findByRole("option", { name: "Jun Park" });
    await act(async () => {
      fireEvent.click(option);
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Transfer" }));
    });
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog.textContent).toContain("removes the billing link");
  });
});

describe("leaving and deleting an organization", () => {
  const NOT_FOUND_NOTICE =
    "It was not found, or you no longer have access to it.";

  /**
   * A transport with the latency of a real control plane: ListOrganizations
   * answers slowly, so a refetch of it is still running when the others
   * have settled.
   */
  function slowTransport(fail: string[] = [], userId?: string) {
    const mock = createMockTransport({ userId });
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        await new Promise((resolve) =>
          setTimeout(resolve, method.name === "ListOrganizations" ? 80 : 20),
        );
        if (fail.includes(method.name)) {
          throw new ConnectError("the request failed", Code.Unavailable);
        }
        return mock.unary(method, ...rest);
      },
    };
    return transport;
  }

  /** Records whether the not-found notice was ever in the document. */
  function watchNotice() {
    const seen = { notice: false };
    const check = () => {
      if (document.body.textContent?.includes(NOT_FOUND_NOTICE)) {
        seen.notice = true;
      }
    };
    const observer = new MutationObserver(check);
    observer.observe(document.body, {
      childList: true,
      subtree: true,
      characterData: true,
    });
    return { seen, stop: () => observer.disconnect() };
  }

  const confirmIn = async (label: string) => {
    const dialog = await screen.findByRole("alertdialog");
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: label }));
    });
    return dialog;
  };

  test("leaving as a member never shows the not-found notice and ends at /", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/members`, {
      transport: slowTransport(),
      queryClient: createQueryClient(),
    });
    const watcher = watchNotice();
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Leave organization" }),
      );
    });
    await confirmIn("Leave organization");
    await settle(400);
    watcher.stop();
    expect(watcher.seen.notice).toBe(false);
    expect(window.location.pathname).toBe("/");
    expect(
      await screen.findByRole("link", { name: /Acme Robotics/ }),
    ).toBeTruthy();
    // The organization list was refetched after the navigation.
    expect(screen.queryByRole("link", { name: /Northwind Labs/ })).toBeNull();
  });

  test("leaving as an admin never shows the not-found notice and ends at /", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, {
      transport: slowTransport([], "user-admin-jun"),
      userId: "user-admin-jun",
      queryClient: createQueryClient(),
    });
    const watcher = watchNotice();
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Leave organization" }),
      );
    });
    await confirmIn("Leave organization");
    await settle(400);
    watcher.stop();
    expect(watcher.seen.notice).toBe(false);
    expect(window.location.pathname).toBe("/");
  });

  test("deleting as the owner never shows the not-found notice and ends at /", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      transport: slowTransport(),
      queryClient: createQueryClient(),
    });
    const watcher = watchNotice();
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Delete organization" }),
      );
    });
    await confirmIn("Delete organization");
    await settle(400);
    watcher.stop();
    expect(watcher.seen.notice).toBe(false);
    expect(window.location.pathname).toBe("/");
    expect(
      await screen.findByRole("link", { name: /Northwind Labs/ }),
    ).toBeTruthy();
    expect(screen.queryByRole("link", { name: /Acme Robotics/ })).toBeNull();
  });

  test("a failed leave shows the error in the dialog and keeps the path", async () => {
    const path = `/orgs/${MOCK_IDS.northwind}/members`;
    renderConsole(path, {
      transport: slowTransport(["RemoveMember"]),
      queryClient: createQueryClient(),
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Leave organization" }),
      );
    });
    const dialog = await confirmIn("Leave organization");
    await settle(200);
    expect(dialog.querySelector(".notice-error")).not.toBeNull();
    expect(window.location.pathname).toBe(path);
  });

  test("a failed delete shows the error in the dialog and keeps the path", async () => {
    const path = `/orgs/${MOCK_IDS.acme}/settings`;
    renderConsole(path, {
      transport: slowTransport(["DeleteOrganization"]),
      queryClient: createQueryClient(),
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Delete organization" }),
      );
    });
    const dialog = await confirmIn("Delete organization");
    await settle(200);
    expect(dialog.querySelector(".notice-error")).not.toBeNull();
    expect(window.location.pathname).toBe(path);
  });

  test("removing another member refreshes the members list", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, {
      transport: slowTransport(),
      queryClient: createQueryClient(),
    });
    const row = (await screen.findByText("user-member-ada")).closest("li")!;
    await act(async () => {
      fireEvent.click(within(row).getByRole("button", { name: /^Remove/ }));
    });
    await confirmIn("Remove member");
    await settle(300);
    expect(screen.queryByText("user-member-ada")).toBeNull();
    expect(screen.getByText("Jun Park")).toBeTruthy();
    expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.acme}/members`);
  });
});

describe("ended session", () => {
  /** A transport that fails one method with the given code. */
  function failing(methodName: string, code: Code): Transport {
    const mock = createMockTransport();
    return {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === methodName) {
          throw new ConnectError("x", code);
        }
        return mock.unary(method, ...rest);
      },
    };
  }

  test("offers sign-in instead of Try again when listing organizations fails with unauthenticated", async () => {
    const { signIns } = renderConsole("/", {
      transport: failing("ListOrganizations", Code.Unauthenticated),
    });
    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Sign in with Delino" }),
      );
    });
    expect(signIns).toHaveLength(1);
  });

  test("offers sign-in instead of Try again when loading an organization fails with unauthenticated", async () => {
    const { signIns } = renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport: failing("GetOrganization", Code.Unauthenticated),
    });
    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Sign in with Delino" }),
      );
    });
    expect(signIns).toHaveLength(1);
  });

  test("shows an alert and keeps the sign-in button usable when the sign-in did not start", async () => {
    const { signIns } = renderConsole("/", {
      transport: failing("ListOrganizations", Code.Unauthenticated),
      signInFailed: true,
    });
    expect(
      await screen.findByText(
        "Sign-in did not start. Check your connection and try again.",
      ),
    ).toBeTruthy();
    const signIn = screen.getByRole("button", {
      name: "Sign in with Delino",
    }) as HTMLButtonElement;
    expect(signIn.disabled).toBe(false);
    await act(async () => {
      fireEvent.click(signIn);
    });
    expect(signIns).toHaveLength(1);
  });

  test("keeps the invitation secret for a retry after the sign-in did not start", async () => {
    const { signIns } = renderConsole(
      `/invitations/accept#secret=${MOCK_INVITATION_SECRET}`,
      {
        transport: failing("AcceptInvitation", Code.Unauthenticated),
        signInFailed: true,
      },
    );
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(
      (
        await screen.findByText(
          "Sign-in did not start. Check your connection and try again.",
        )
      ).getAttribute("role"),
    ).toBe("alert");
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Sign in with Delino" }),
      );
    });
    expect(signIns).toHaveLength(1);
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });

  test("keeps Try again and offers no sign-in when the failure is unavailable", async () => {
    renderConsole("/", {
      transport: failing("ListOrganizations", Code.Unavailable),
    });
    expect(
      await screen.findByRole("button", { name: "Try again" }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Sign in with Delino" }),
    ).toBeNull();
  });

  test("keeps the invitation secret across the sign-in after the accept request fails with unauthenticated", async () => {
    const { signIns } = renderConsole(
      `/invitations/accept#secret=${MOCK_INVITATION_SECRET}`,
      { transport: failing("AcceptInvitation", Code.Unauthenticated) },
    );
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    // The accept handler deleted the stored copy.
    expect(pendingInvitation.read()).toBeUndefined();
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Sign in with Delino" }),
      );
    });
    expect(signIns).toHaveLength(1);
    expect(pendingInvitation.read()).toBe(MOCK_INVITATION_SECRET);
  });
});

describe("a failed refetch of loaded data", () => {
  const REFRESH_NOTICE = /The last refresh failed/;

  /** A transport that fails the listed methods with `code` while `failing.code` is set. */
  function switchable() {
    const mock = createMockTransport();
    const state: { code: Code | undefined; methods: string[] } = {
      code: undefined,
      methods: [],
    };
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (state.code !== undefined && state.methods.includes(method.name)) {
          throw new ConnectError("the request failed", state.code);
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, state };
  }

  function newQueryClient() {
    return new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
  }

  const refetchAll = async (queryClient: QueryClient) => {
    await act(async () => {
      await queryClient.refetchQueries();
    });
    await settle(60);
  };

  test.each([
    new ConnectError("the request failed", Code.Unavailable),
    new TypeError("Failed to fetch"),
  ])(
    "keeps loaded organizations and retries a transient refresh error: %s",
    async (error) => {
      const { transport, calls } = holdPagedTransport(["ListOrganizations"]);
      const queryClient = newQueryClient();
      renderConsole("/", { transport, queryClient });
      await waitFor(() => expect(calls.ListOrganizations).toHaveLength(1));
      await act(async () => calls.ListOrganizations![0]!.settle());
      const list = await screen.findByRole("list", {
        name: "Your organizations",
      });

      const refetch = queryClient.refetchQueries();
      await waitFor(() => expect(calls.ListOrganizations).toHaveLength(2));
      await act(async () => {
        calls.ListOrganizations![1]!.settle(error);
        await refetch;
      });

      const notice = await screen.findByText(REFRESH_NOTICE);
      expect(
        notice.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
      expect(
        within(list).getByRole("link", { name: /Acme Robotics/ }),
      ).toBeTruthy();
      expect(
        within(list).getByRole("link", { name: /Northwind Labs/ }),
      ).toBeTruthy();

      const retry = screen.getByRole<HTMLButtonElement>("button", {
        name: "Try again",
      });
      expect(retry.disabled).toBe(false);
      await act(async () => fireEvent.click(retry));
      await waitFor(() => expect(retry.disabled).toBe(true));
      expect(calls.ListOrganizations).toHaveLength(3);
      expect(screen.getByRole("list", { name: "Your organizations" })).toBe(
        list,
      );

      await act(async () => calls.ListOrganizations![2]!.settle());
      await waitFor(() =>
        expect(screen.queryByText(REFRESH_NOTICE)).toBeNull(),
      );
      expect(screen.getByRole("list", { name: "Your organizations" })).toBe(
        list,
      );
    },
  );

  test.each([
    [Code.NotFound, "It was not found, or you no longer have access to it."],
    [Code.PermissionDenied, "You do not have permission to do this."],
    [Code.Unauthenticated, "Your session ended. Sign in again."],
  ] as const)(
    "replaces the loaded organization list on terminal error %s",
    async (code, message) => {
      const { transport, state } = switchable();
      const queryClient = newQueryClient();
      renderConsole("/", { transport, queryClient });
      await screen.findByRole("list", { name: "Your organizations" });

      state.code = code;
      state.methods = ["ListOrganizations"];
      await refetchAll(queryClient);

      expect(await screen.findByText(message)).toBeTruthy();
      expect(
        screen.queryByRole("list", { name: "Your organizations" }),
      ).toBeNull();
      expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
      if (code === Code.Unauthenticated) {
        expect(
          screen.getByRole("button", { name: "Sign in with Delino" }),
        ).toBeTruthy();
      }
    },
  );

  test("shows a full error when the first organization request fails", async () => {
    const { transport, state } = switchable();
    state.code = Code.Unavailable;
    state.methods = ["ListOrganizations"];
    renderConsole("/", { transport });

    expect(
      await screen.findByText(
        "OwnSand is not available right now. Try again in a moment.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
    expect(
      screen.queryByRole("list", { name: "Your organizations" }),
    ).toBeNull();
    expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
  });

  test("keeps the page, the navigation and an open dialog with its typed value, and clears the notice on the next success", async () => {
    const { transport, state } = switchable();
    const queryClient = newQueryClient();
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, {
      transport,
      queryClient,
    });
    await screen.findByText(/ci-runner/);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create API key" }));
    });
    await settle(60);
    const input = screen.getByRole("textbox", { name: "Name" });
    await act(async () => {
      fireEvent.input(input, { target: { value: "deploy-bot" } });
    });
    const nav = document.querySelector(".sidenav");
    expect(nav).not.toBeNull();

    state.code = Code.Unavailable;
    state.methods = ["GetOrganization", "ListApiKeys", "GetBillingStatus"];
    await refetchAll(queryClient);

    // The page, the sidenav, the dialog and the typed value stay mounted.
    expect((await screen.findAllByText(REFRESH_NOTICE)).length).toBeGreaterThan(
      0,
    );
    expect(document.querySelector(".sidenav")).toBe(nav);
    expect(screen.getByText(/ci-runner/)).toBeTruthy();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByRole("textbox", { name: "Name" })).toBe(input);
    expect((input as HTMLInputElement).value).toBe("deploy-bot");

    // Try again succeeds once the failure is gone, and removes the notice.
    state.code = undefined;
    const retry = screen.getAllByRole("button", {
      name: "Try again",
      hidden: true,
    })[0]!;
    await act(async () => {
      fireEvent.click(retry);
    });
    await settle(60);
    await refetchAll(queryClient);
    expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
    expect(screen.getByRole("textbox", { name: "Name" })).toBe(input);
    expect((input as HTMLInputElement).value).toBe("deploy-bot");
  });

  const pages = [
    {
      name: "sandboxes",
      path: "sandboxes",
      method: "ListSandboxes",
      text: shortIdText(MOCK_IDS.runningSandbox),
    },
    {
      name: "sandbox",
      path: `sandboxes/${MOCK_IDS.runningSandbox}`,
      method: "GetSandbox",
      text: "Sandbox",
    },
    {
      name: "snapshots",
      path: "snapshots",
      method: "ListSnapshots",
      text: "Templates",
    },
    {
      name: "devices",
      path: "devices",
      method: "ListDevices",
      text: "build-box",
    },
    {
      name: "device",
      path: `devices/${MOCK_IDS.buildBox}`,
      method: "GetDevice",
      text: "Save limits",
    },
    {
      name: "members",
      path: "members",
      method: "ListMembers",
      text: "Invitations",
    },
    {
      name: "enrollment tokens",
      path: "enrollment-tokens",
      method: "ListEnrollmentTokens",
      text: "Create token",
    },
    {
      name: "settings",
      path: "settings",
      method: "GetAgentVersionPolicy",
      text: "Agent version policy",
    },
    {
      name: "billing section",
      path: "settings",
      method: "GetBillingStatus",
      text: "Billing",
    },
  ];

  for (const page of pages) {
    test(`keeps the ${page.name} page for a transient failure and replaces it for not_found`, async () => {
      const { transport, state } = switchable();
      const queryClient = newQueryClient();
      renderConsole(`/orgs/${MOCK_IDS.acme}/${page.path}`, {
        transport,
        queryClient,
      });
      await screen.findAllByText(page.text, { exact: false });
      await settle(60);

      state.code = Code.Unavailable;
      state.methods = [page.method];
      await refetchAll(queryClient);
      expect(
        (await screen.findAllByText(REFRESH_NOTICE)).length,
      ).toBeGreaterThan(0);
      expect(
        screen.getAllByText(page.text, { exact: false }).length,
      ).toBeGreaterThan(0);
      expect(
        screen.getByRole("navigation", { name: "Organization" }),
      ).toBeTruthy();

      // A terminal error replaces the content, with no stale data left.
      state.code = Code.NotFound;
      await refetchAll(queryClient);
      expect(
        (
          await screen.findAllByText(
            "It was not found, or you no longer have access to it.",
          )
        ).length,
      ).toBeGreaterThan(0);
      expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
    });
  }

  test("replaces the whole layout when the organization fails with a terminal error after loading", async () => {
    for (const code of [
      Code.NotFound,
      Code.PermissionDenied,
      Code.Unauthenticated,
    ]) {
      cleanup();
      const { transport, state } = switchable();
      const queryClient = newQueryClient();
      renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, {
        transport,
        queryClient,
      });
      await screen.findByText(/ci-runner/);
      state.code = code;
      state.methods = ["GetOrganization"];
      await refetchAll(queryClient);
      expect(
        screen.queryByRole("navigation", { name: "Organization" }),
      ).toBeNull();
      expect(screen.queryByText(/ci-runner/)).toBeNull();
      expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
      expect(
        (await screen.findAllByRole("alert")).some((el) =>
          el.className.includes("notice"),
        ),
      ).toBe(true);
    }
  });

  test("shows the full error, without the page, when the first load fails", async () => {
    const { transport, state } = switchable();
    state.code = Code.Unavailable;
    state.methods = ["GetOrganization"];
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    expect(
      await screen.findByText(
        "OwnSand is not available right now. Try again in a moment.",
      ),
    ).toBeTruthy();
    expect(
      screen.queryByRole("navigation", { name: "Organization" }),
    ).toBeNull();
    expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
  });

  test("shows the full list error, without a stale list, when the first list load fails", async () => {
    const { transport, state } = switchable();
    state.code = Code.Unavailable;
    state.methods = ["ListApiKeys"];
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    expect(
      await screen.findByText(
        "OwnSand is not available right now. Try again in a moment.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByRole("navigation", { name: "Organization" }),
    ).toBeTruthy();
    expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
    expect(screen.queryByText(/ci-runner/)).toBeNull();
  });
});

describe("enrollment token expiry", () => {
  afterEach(() => {
    rs.useRealTimers();
  });

  test("a refreshed list shows an expired token as Expired without a Revoke button", async () => {
    const t0 = Date.UTC(2026, 0, 1, 12, 0, 0);
    // Only Date is faked: the page and its queries keep real timers. The UI
    // offers whole-hour lifetimes, so the test creates a one-second token
    // through the same transport and moves the clock past it.
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(t0);
    const transport = createMockTransport();
    const { token } = await createClient(
      DeviceService,
      transport,
    ).createEnrollmentToken({
      organizationId: MOCK_IDS.acme,
      ttl: { seconds: 1n },
    });
    const rowOf = async () =>
      within(await screen.findByRole("list", { name: "Enrollment tokens" }))
        .getAllByRole("listitem")
        .find((row) => row.textContent?.includes(token!.tokenPrefix));

    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    const pending = await rowOf();
    expect(pending?.textContent).toContain("Not used");
    expect(
      within(pending as HTMLElement).getByRole("button", {
        name: /^Revoke/,
      }),
    ).toBeTruthy();

    cleanup();
    rs.setSystemTime(t0 + 2000);
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    const expired = (await rowOf()) as HTMLElement;
    expect(expired.textContent).toContain("Expired");
    expect(within(expired).queryByRole("button", { name: /^Revoke/ })).toBe(
      null,
    );
  });
});
