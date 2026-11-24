// SPDX-License-Identifier: Apache-2.0
import { timestampMs } from "@bufbuild/protobuf/wkt";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
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
import { createQueryOptions } from "@connectrpc/connect-query";
import { ApiKeyService } from "@delino/tide-protos/v1/api_key_pb";
import { AuditService } from "@delino/tide-protos/v1/audit_pb";
import { Role } from "@delino/tide-protos/v1/common_pb";
import { GitHubService } from "@delino/tide-protos/v1/github_pb";
import { RunnerProfileService } from "@delino/tide-protos/v1/runner_profile_pb";
import { QueryClient } from "@tanstack/react-query";
import { MAX_PAGE_SIZE, PAGE_SIZE } from "./api.ts";
import { Console } from "./App.tsx";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { ErrorNotice } from "./components/ui.tsx";
import { AuthMode } from "./config.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
  MOCK_USER,
  type MockTransportOptions,
} from "./mock/mock-api.ts";
import { organizationPages } from "./mock/organization-pages.ts";
import { externalNavigation } from "./navigation.ts";

const config = {
  apiOrigin: "https://api.test",
  authMode: AuthMode.Mock,
  logto: undefined,
};

function renderConsole(
  path: string,
  options: {
    signedIn?: boolean;
    signIn?: Auth["signIn"];
    transport?: Transport;
    mock?: MockTransportOptions;
    staleTime?: number;
  } = {},
) {
  window.history.replaceState(null, "", path);
  const signedIn = options.signedIn ?? true;
  const auth: Auth = {
    isLoading: false,
    isAuthenticated: signedIn,
    userId: signedIn ? MOCK_USER : undefined,
    signIn: options.signIn ?? (async () => {}),
    signOut: async () => {},
    getAccessToken: async () => "token",
  };
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: options.staleTime },
      mutations: { retry: false },
    },
  });
  render(
    <AuthContextProvider value={auth}>
      <Console
        config={config}
        transport={options.transport ?? createMockTransport(options.mock)}
        queryClient={queryClient}
      />
    </AuthContextProvider>,
  );
  return queryClient;
}

async function settle(ms = 40) {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}

async function click(element: HTMLElement) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle();
}

async function type(element: HTMLElement, value: string) {
  await act(async () => {
    fireEvent.input(element, { target: { value } });
  });
}

/** Every value that the console keeps in browser storage. */
function storedText(): string {
  return JSON.stringify({ ...window.sessionStorage, ...window.localStorage });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("organizations", () => {
  test("shares all pages in server order between home and the switcher", async () => {
    const { transport, listRequests } = await organizationPages({
      lastRole: Role.ADMIN,
    });
    renderConsole("/", { transport, staleTime: 10_000 });

    const list = await screen.findByRole("list", {
      name: "Your organizations",
    });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(201);
    expect(rows[0].textContent).toContain("Acme Robotics");
    expect(rows[199].textContent).toContain("Fixture organization 200");
    expect(rows[200].textContent).toContain("Globex");
    expect(within(list).getAllByRole("link", { name: /Globex/ })).toHaveLength(
      1,
    );

    await click(within(list).getByRole("link", { name: /Acme Robotics/ }));
    await click(await screen.findByRole("combobox", { name: "Organization" }));
    await click(await screen.findByRole("option", { name: "Globex" }));

    expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.globex}`);
    expect(
      listRequests.map(({ pageSize, pageToken }) => [pageSize, pageToken]),
    ).toEqual([
      [200, ""],
      [200, "after-200"],
    ]);
  });

  test("lists the caller's organizations and creates one from a focused dialog", async () => {
    renderConsole("/");
    expect(
      await screen.findByRole("link", { name: /Acme Robotics/ }),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: /Northwind Labs/ }).textContent,
    ).toContain("Member");
    await click(screen.getByRole("button", { name: "Create organization" }));
    await settle(60);
    const input = screen.getByRole("textbox", { name: "Organization name" });
    expect(document.activeElement).toBe(input);
    await type(input, "Fabrikam");
    await click(screen.getByRole("button", { name: "Create" }));
    await settle(80);
    expect(window.location.pathname).toMatch(/^\/orgs\/[0-9a-f-]+$/);
    expect(await screen.findByText("Set up tide")).toBeTruthy();
  });

  test("links to account settings and to the OwnSand console", async () => {
    renderConsole("/");
    await screen.findByRole("link", { name: /Acme Robotics/ });
    expect(
      screen
        .getByRole("link", { name: "Account settings" })
        .getAttribute("href"),
    ).toBe("https://accounts.delino.io");
    expect(
      screen
        .getByRole("link", { name: "OwnSand console" })
        .getAttribute("href"),
    ).toBe("https://ownsand.delino.io");
  });
});

describe("failed requests", () => {
  function failingTransport(methodName: string, code: Code): Transport {
    const mock = createMockTransport();
    return {
      ...mock,
      async unary(method, ...args) {
        if (method.name === methodName) {
          throw new ConnectError("Request failed", code);
        }
        return mock.unary(method, ...args);
      },
    };
  }

  test.each([
    ["/", "ListOrganizations"],
    [`/orgs/${MOCK_IDS.acme}`, "GetOrganization"],
  ])("offers sign-in when the session ends on %s", async (path, method) => {
    const signIn = rs.fn(async () => {});
    renderConsole(path, {
      signIn,
      transport: failingTransport(method, Code.Unauthenticated),
    });

    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(signIn).not.toHaveBeenCalled();

    const button = screen.getByRole("button", { name: "Sign in with Delino" });
    await click(button);
    expect(signIn).toHaveBeenCalledTimes(1);
    await click(button);
    expect(signIn).toHaveBeenCalledTimes(2);
  });

  test("offers retry when the API is unavailable", async () => {
    renderConsole("/", {
      transport: failingTransport("ListOrganizations", Code.Unavailable),
    });

    expect(
      await screen.findByRole("button", { name: "Try again" }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Sign in with Delino" }),
    ).toBeNull();
  });

  test("renders a generic notice without an auth provider", () => {
    const retry = rs.fn();
    render(<ErrorNotice error={new Error("Request failed")} onRetry={retry} />);

    expect(screen.getByText("Something went wrong. Try again.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(retry).toHaveBeenCalledTimes(1);
  });
});

describe("roles", () => {
  test("a member does not see admin sections or actions", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/profiles`);
    const nav = await screen.findByRole("navigation", { name: "Organization" });
    await screen.findByLabelText("runs-on label of mac-build");
    expect(nav.textContent).not.toContain("Settings");
    expect(
      screen.queryByRole("button", { name: "Create runner profile" }),
    ).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  test("a member who opens settings sees no settings", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/settings`);
    expect(
      await screen.findByText("You do not have permission to open this page."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save limit" })).toBeNull();
  });
});

describe("OwnSand link", () => {
  test("an INVALID link shows a banner on every organization route", async () => {
    for (const section of ["", "/requests", "/members"]) {
      renderConsole(`/orgs/${MOCK_IDS.northwind}${section}`);
      const banner = await screen.findByRole("alert", { name: "OwnSand link" });
      expect(banner.textContent).toContain("Dispatch has stopped");
      expect(
        within(banner)
          .getByRole("link", { name: "Go to integrations" })
          .getAttribute("href"),
      ).toBe(`/orgs/${MOCK_IDS.northwind}/integrations`);
      cleanup();
    }
  });

  test("links OwnSand with a password field that is cleared, then shows only when the key was set", async () => {
    renderConsole(`/orgs/${MOCK_IDS.globex}/integrations`);
    await click(await screen.findByRole("button", { name: "Link OwnSand" }));
    await settle(60);
    const dialog = screen.getByRole("dialog", { name: "Link OwnSand" });
    expect(document.activeElement).toBe(
      within(dialog).getByRole("textbox", { name: "OwnSand organization ID" }),
    );
    const key = dialog.querySelector<HTMLInputElement>(
      'input[type="password"]',
    );
    expect(key).not.toBeNull();
    await type(
      within(dialog).getByRole("textbox", { name: "OwnSand organization ID" }),
      "0193a000-0000-7000-8000-00000000aaaa",
    );
    await type(key!, "osk_secret_value_123");
    await click(within(dialog).getByRole("button", { name: "Link" }));
    await settle(80);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(await screen.findByText(/^Set at /)).toBeTruthy();
    expect(document.body.textContent).not.toContain("osk_secret_value_123");
    expect(storedText()).not.toContain("osk_secret_value_123");
    // The replace dialog opens empty and focuses its only input.
    await click(screen.getByRole("button", { name: "Replace API key" }));
    await settle(60);
    const replace = screen.getByRole("dialog", { name: "Replace the API key" });
    const input = replace.querySelector<HTMLInputElement>(
      'input[type="password"]',
    )!;
    expect(input.value).toBe("");
    expect(document.activeElement).toBe(input);
    await act(async () => {
      fireEvent.keyDown(replace, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("removing the link needs a confirmation", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`);
    await click(await screen.findByRole("button", { name: "Remove link" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog.textContent).toContain("deletes every runner");
    await click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Remove link" })).toBeTruthy();
  });
});

describe("integrations", () => {
  async function editClaudeIntegration() {
    renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`);
    const list = within(
      await screen.findByRole("list", { name: "Agent integrations" }),
    );
    const row = list.getByText("claude-team · Claude Code").closest("li")!;
    await click(within(row).getByRole("button", { name: "Edit" }));
    const dialog = screen.getByRole("dialog", { name: "Edit claude-team" });
    const form = within(dialog);
    const name = form.getByRole("textbox", { name: "Name" });
    const save = () =>
      click(form.getByRole("button", { name: "Save changes" }));
    return { list, dialog, form, name, save };
  }

  test("keeps the edit dialog open when the name already exists", async () => {
    const { list, dialog, form, name, save } = await editClaudeIntegration();
    await type(name, "codex-prod");
    await save();

    expect(
      await form.findByText("an agent integration with this name exists"),
    ).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Edit claude-team" })).toBe(
      dialog,
    );
    expect(list.getAllByText(/^codex-prod · /)).toHaveLength(1);
    expect(list.getAllByText(/^claude-team · /)).toHaveLength(1);
  });

  test("renames an integration to an unused name", async () => {
    const { list, name, save } = await editClaudeIntegration();
    await type(name, "claude-renamed");
    await save();

    expect(await list.findByText("claude-renamed · Claude Code")).toBeTruthy();
    expect(list.queryByText("claude-team · Claude Code")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("closes the edit dialog when the integration name is unchanged", async () => {
    const { list, save } = await editClaudeIntegration();
    await save();

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(list.getByText("claude-team · Claude Code")).toBeTruthy();
  });

  test("shows the OpenAI webhook URL and the credential state without values", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`);
    const url = await screen.findByLabelText("webhook URL of codex-prod");
    expect(url.textContent).toBe(
      `https://tide.api.delino.io/webhooks/openai/${MOCK_IDS.codexIntegration}`,
    );
    expect(screen.getAllByText(/Webhook secret: Set at/).length).toBe(1);
  });

  test("creates a Devin integration with password credential fields", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`);
    await click(
      await screen.findByRole("button", { name: "Create agent integration" }),
    );
    await settle(60);
    const dialog = screen.getByRole("dialog", {
      name: "Create an agent integration",
    });
    expect(document.activeElement).toBe(
      within(dialog).getByRole("textbox", { name: "Name" }),
    );
    // Claude Code is the default vendor: one environment key.
    expect(dialog.querySelectorAll('input[type="password"]').length).toBe(1);
    await type(
      within(dialog).getByRole("textbox", { name: "Name" }),
      "claude-2",
    );
    await type(
      dialog.querySelector<HTMLInputElement>('input[type="password"]')!,
      "env_key_value",
    );
    await click(
      within(dialog).getByRole("button", { name: "Create integration" }),
    );
    await settle(80);
    expect(await screen.findByText(/claude-2/)).toBeTruthy();
    expect(document.body.textContent).not.toContain("env_key_value");
  });

  test("Connect GitHub opens the installation URL from the control plane", async () => {
    const assigned: string[] = [];
    const original = externalNavigation.assign;
    externalNavigation.assign = (url) => assigned.push(url);
    try {
      renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`);
      await click(
        await screen.findByRole("button", { name: "Connect GitHub" }),
      );
      await settle(60);
      expect(assigned.length).toBe(1);
      expect(assigned[0]).toMatch(
        /^https:\/\/github\.com\/apps\/tide\/installations\/new\?state=/,
      );
    } finally {
      externalNavigation.assign = original;
    }
  });
});

describe("GitHub setup callback", () => {
  const callbackPath =
    "/github/setup?installation_id=51000001&code=gh_code&state=mock-state&setup_action=install";
  const integrationsPath = `/orgs/${MOCK_IDS.globex}/integrations`;
  const installationName = /github-org-51000001/;

  function holdLinkRequest() {
    const mock = createMockTransport();
    let completeLink!: (error?: ConnectError) => void;
    const response = new Promise<ConnectError | undefined>((resolve) => {
      completeLink = resolve;
    });
    const linkRequest = rs.fn();
    const transport: Transport = {
      ...mock,
      async unary(method, ...args) {
        if (method.name === GitHubService.method.linkGitHubInstallation.name) {
          linkRequest();
          const error = await response;
          if (error !== undefined) throw error;
        }
        return mock.unary(method, ...args);
      },
    };
    return { transport, completeLink, linkRequest };
  }

  async function submitToGlobex() {
    await click(
      await screen.findByRole("combobox", { name: "tide organization" }),
    );
    await click(await screen.findByRole("option", { name: /Globex/ }));
    await click(screen.getByRole("button", { name: "Link installation" }));
  }

  test("waits for the last page before offering every eligible organization", async () => {
    let releaseLastPage!: () => void;
    const lastPage = new Promise<void>((resolve) => {
      releaseLastPage = resolve;
    });
    const { transport, listRequests, linkRequests } = await organizationPages({
      firstRole: Role.OWNER,
      lastRole: Role.ADMIN,
      beforeLastPage: () => lastPage,
    });
    renderConsole(callbackPath, { transport });

    await waitFor(() => expect(listRequests).toHaveLength(2));
    expect(
      screen.getByRole("status", { name: "Loading your organizations" }),
    ).toBeTruthy();
    expect(linkRequests).toHaveLength(0);
    expect(window.location.search).toBe("");

    releaseLastPage();
    await click(
      await screen.findByRole("combobox", { name: "tide organization" }),
    );
    expect(
      screen.getAllByRole("option").map((option) => option.textContent),
    ).toEqual(["Acme Robotics (owner)", "Globex (admin)"]);
    await click(screen.getByRole("option", { name: "Globex (admin)" }));
    await click(screen.getByRole("button", { name: "Link installation" }));

    expect(await screen.findByText(installationName)).toBeTruthy();
    expect(window.location.pathname).toBe(integrationsPath);
    expect(linkRequests.map((request) => request.organizationId)).toEqual([
      MOCK_IDS.globex,
    ]);
  });

  test("retries a failed later page before linking to the sole eligible organization", async () => {
    let attempts = 0;
    const { transport, listRequests, linkRequests } = await organizationPages({
      lastRole: Role.ADMIN,
      beforeLastPage: async () => {
        if (++attempts === 1)
          throw new ConnectError("Page failed", Code.Internal);
      },
    });
    renderConsole(callbackPath, { transport });

    const retry = await screen.findByRole("button", { name: "Try again" });
    expect(within(screen.getByRole("main")).getByRole("alert")).toBeTruthy();
    expect(window.location.search).toBe("");
    expect(linkRequests).toHaveLength(0);
    await click(retry);

    expect(await screen.findByText(installationName)).toBeTruthy();
    expect(window.location.pathname).toBe(integrationsPath);
    expect(linkRequests).toMatchObject([
      {
        organizationId: MOCK_IDS.globex,
        githubInstallationId: 51000001n,
        code: "gh_code",
        state: "mock-state",
      },
    ]);
    expect(listRequests.slice(0, 4).map(({ pageToken }) => pageToken)).toEqual([
      "",
      "after-200",
      "",
      "after-200",
    ]);
  });

  test("keeps a full first page when the terminal page is empty", async () => {
    const { transport, listRequests, linkRequests } = await organizationPages(
      {},
    );
    renderConsole(callbackPath, { transport, staleTime: 10_000 });

    expect(await screen.findByText(/Only an owner or admin/)).toBeTruthy();
    expect(listRequests.map(({ pageToken }) => pageToken)).toEqual([
      "",
      "after-200",
    ]);
    expect(linkRequests).toHaveLength(0);

    await click(screen.getByRole("link", { name: "Go to your organizations" }));
    const list = await screen.findByRole("list", {
      name: "Your organizations",
    });
    expect(within(list).getAllByRole("listitem")).toHaveLength(200);
    expect(listRequests).toHaveLength(2);
  });

  test("strips callback values and replaces the setup entry on success", async () => {
    const { transport, completeLink, linkRequest } = holdLinkRequest();
    renderConsole(callbackPath, { transport });
    await submitToGlobex();
    expect(window.location.search).toBe("");
    expect(linkRequest).toHaveBeenCalledTimes(1);
    const historyLength = window.history.length;

    completeLink();

    expect(await screen.findByText(installationName)).toBeTruthy();
    expect(window.location.pathname).toBe(integrationsPath);
    expect(window.history.length).toBe(historyLength);
    expect(linkRequest).toHaveBeenCalledTimes(1);
  });

  test.each(["Tide link", "Back", "return to setup"] as const)(
    "preserves the current entry after leaving with %s and refreshes Integrations",
    async (navigation) => {
      const { transport, completeLink, linkRequest } = holdLinkRequest();
      window.history.replaceState({ entry: "organizations" }, "", "/");
      window.history.pushState(null, "", callbackPath);
      const queryClient = renderConsole(callbackPath, { transport });
      const installations = createQueryOptions(
        GitHubService.method.listGitHubInstallations,
        { organizationId: MOCK_IDS.globex, pageSize: MAX_PAGE_SIZE },
        { transport },
      );
      queryClient.setQueryDefaults(installations.queryKey, {
        staleTime: Infinity,
      });
      const before = await queryClient.fetchQuery(installations);
      expect(before.installations).toHaveLength(0);
      await submitToGlobex();
      expect(linkRequest).toHaveBeenCalledTimes(1);

      if (navigation === "Back") {
        await act(async () => window.history.back());
      } else {
        await click(screen.getByRole("link", { name: "tide" }));
      }
      expect(
        await screen.findByRole("heading", { name: "Organizations" }),
      ).toBeTruthy();
      if (navigation === "return to setup") {
        await act(async () => window.history.back());
        expect(
          await screen.findByText(/has no GitHub installation details/),
        ).toBeTruthy();
      }
      const currentPath =
        navigation === "return to setup" ? "/github/setup" : "/";
      const currentEntry = { entry: navigation };
      window.history.replaceState(currentEntry, "", currentPath);
      const historyLength = window.history.length;

      completeLink();
      await waitFor(() => {
        expect(queryClient.getMutationCache().getAll()[0]?.state.status).toBe(
          "success",
        );
      });

      expect(window.location.pathname).toBe(currentPath);
      expect(window.history.state).toEqual(currentEntry);
      expect(window.history.length).toBe(historyLength);
      expect(
        queryClient.getQueryState(installations.queryKey)?.isInvalidated,
      ).toBe(true);
      if (navigation === "return to setup") {
        await click(
          screen.getByRole("link", { name: "Go to your organizations" }),
        );
      }
      await click(await screen.findByRole("link", { name: /Globex/ }));
      await click(await screen.findByRole("link", { name: "Integrations" }));
      expect(await screen.findByText(installationName)).toBeTruthy();
      expect(linkRequest).toHaveBeenCalledTimes(1);
    },
  );

  test("checks the address before the router renders a navigation", async () => {
    const { transport, completeLink, linkRequest } = holdLinkRequest();
    const queryClient = renderConsole(callbackPath, { transport });
    await submitToGlobex();
    expect(linkRequest).toHaveBeenCalledTimes(1);
    // Change the browser entry before the router receives its popstate event.
    window.history.pushState({ entry: "organizations" }, "", "/");
    completeLink();
    await waitFor(() => {
      expect(queryClient.getMutationCache().getAll()[0]?.state.status).toBe(
        "success",
      );
    });

    expect(window.location.pathname).toBe("/");
    expect(window.history.state).toEqual({ entry: "organizations" });
  });

  test("keeps setup open and shows restart instructions after a failure", async () => {
    const { transport, completeLink, linkRequest } = holdLinkRequest();
    renderConsole(callbackPath, { transport });
    await submitToGlobex();
    expect(linkRequest).toHaveBeenCalledTimes(1);
    const historyLength = window.history.length;

    completeLink(
      new ConnectError(
        "The installation state expired",
        Code.FailedPrecondition,
      ),
    );

    const message = await screen.findByText("The installation state expired");
    expect(message.closest('[role="alert"]')).toBeTruthy();
    expect(
      screen.getByText(
        /The installation link is single-use. Start the installation again/,
      ),
    ).toBeTruthy();
    expect(window.location.pathname).toBe("/github/setup");
    expect(window.history.length).toBe(historyLength);
    expect(linkRequest).toHaveBeenCalledTimes(1);
  });

  test("an expired issued state keeps the callback on setup and shows restart guidance", async () => {
    const transport = createMockTransport();
    const github = createClient(GitHubService, transport);
    const { url, expireTime } = await github.createGitHubInstallUrl({
      organizationId: MOCK_IDS.globex,
    });
    const state = new URL(url).searchParams.get("state");
    if (!state || !expireTime) throw new Error("Expected state and expiry");
    const clock = rs
      .spyOn(Date, "now")
      .mockReturnValue(timestampMs(expireTime));
    try {
      const query = new URLSearchParams({
        installation_id: "51000002",
        code: "fixture-code",
        state,
      });
      renderConsole(`/github/setup?${query}`, { transport });
      await click(
        await screen.findByRole("combobox", { name: "tide organization" }),
      );
      await click(await screen.findByRole("option", { name: /Globex/ }));
      await click(screen.getByRole("button", { name: "Link installation" }));

      expect(
        await screen.findByText(
          /Start the installation again from the integrations/,
        ),
      ).toBeTruthy();
      expect(window.location.pathname).toBe("/github/setup");
    } finally {
      clock.mockRestore();
    }
  });

  test("an address without the callback values explains what to do", async () => {
    renderConsole("/github/setup");
    expect(
      await screen.findByText(/has no GitHub installation details/),
    ).toBeTruthy();
  });
});

describe("runner profiles", () => {
  test("shows the runs-on label of GitHub job profiles", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`);
    const label = await screen.findByLabelText("runs-on label of linux-build");
    expect(label.textContent).toBe("tide-linux-build");
  });

  test("rejects a duplicate rename without changing profiles or audit events", async () => {
    const transport = createMockTransport();
    const profiles = createClient(RunnerProfileService, transport);
    const audit = createClient(AuditService, transport);
    const organization = { organizationId: MOCK_IDS.acme };
    const [profilesBefore, auditBefore] = await Promise.all([
      profiles.listRunnerProfiles(organization),
      audit.listAuditEvents(organization),
    ]);

    await expect(
      profiles.updateRunnerProfile({
        ...organization,
        runnerProfileId: MOCK_IDS.linuxProfile,
        profile: { name: "codex-linux" },
        updateMask: { paths: ["name"] },
      }),
    ).rejects.toMatchObject({
      code: Code.AlreadyExists,
      rawMessage: "a runner profile with this name exists",
    });

    const [profilesAfter, auditAfter] = await Promise.all([
      profiles.listRunnerProfiles(organization),
      audit.listAuditEvents(organization),
    ]);
    expect(profilesAfter).toEqual(profilesBefore);
    expect(auditAfter).toEqual(auditBefore);
  });

  test.each(["linux-build", "linux-build-new"])(
    "accepts a name update to %s and returns the runs-on label",
    async (name) => {
      const profiles = createClient(
        RunnerProfileService,
        createMockTransport(),
      );
      const { profile } = await profiles.updateRunnerProfile({
        organizationId: MOCK_IDS.acme,
        runnerProfileId: MOCK_IDS.linuxProfile,
        profile: { name },
        updateMask: { paths: ["name"] },
      });

      expect(profile).toMatchObject({ name, githubLabel: `tide-${name}` });
    },
  );

  test("keeps the edit dialog open and shows the duplicate-name error", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`);
    const profiles = await screen.findByRole("list", {
      name: "Runner profiles",
    });
    const row = within(profiles)
      .getByText(/^linux-build ·/)
      .closest("li")!;
    await click(within(row).getByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", {
      name: "Edit linux-build",
    });
    await type(
      within(dialog).getByRole("textbox", { name: "Name" }),
      "codex-linux",
    );
    await click(within(dialog).getByRole("button", { name: "Save changes" }));

    expect(
      await within(dialog).findByText("a runner profile with this name exists"),
    ).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Edit linux-build" })).toBe(
      dialog,
    );
  });

  test("a new profile can use the default priority of the workload kind", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`);
    await click(
      await screen.findByRole("button", { name: "Create runner profile" }),
    );
    await settle(60);
    const create = screen.getByRole("dialog", {
      name: "Create a runner profile",
    });
    await click(within(create).getByRole("combobox", { name: "Priority" }));
    expect(
      await screen.findByRole("option", {
        name: "Default of the workload kind",
      }),
    ).toBeTruthy();
  });

  test("an edit offers only a concrete priority", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`);
    await click((await screen.findAllByRole("button", { name: "Edit" }))[0]!);
    await settle(60);
    const edit = screen.getByRole("dialog", { name: "Edit linux-build" });
    await click(within(edit).getByRole("combobox", { name: "Priority" }));
    expect(await screen.findByRole("option", { name: "High" })).toBeTruthy();
    expect(
      screen.queryByRole("option", { name: "Default of the workload kind" }),
    ).toBeNull();
  });

  test("requires the workspace path for a dev session profile", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`);
    await click(
      await screen.findByRole("button", { name: "Create runner profile" }),
    );
    await settle(60);
    const dialog = screen.getByRole("dialog", {
      name: "Create a runner profile",
    });
    await type(
      within(dialog).getByRole("textbox", { name: "Name" }),
      "linux-dev",
    );
    await click(
      within(dialog).getByRole("combobox", { name: "Workload kind" }),
    );
    await click(await screen.findByRole("option", { name: "Dev session" }));
    const path = within(dialog).getByRole("textbox", {
      name: "Workspace path",
    });
    await click(within(dialog).getByRole("button", { name: "Create profile" }));
    await settle(60);
    expect(dialog.textContent).toContain("Enter the workspace path");
    await type(path, "/home/dev/work");
    await click(within(dialog).getByRole("button", { name: "Create profile" }));
    await settle(80);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(await screen.findByText(/linux-dev · Dev session/)).toBeTruthy();
  });
});

describe("overview", () => {
  test("shows waiting requests in queue order and the device pool", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}`);
    const table = await screen.findByRole("table", {
      name: "Waiting requests",
    });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.children[2]!.textContent)).toEqual([
      "GitHub job",
      "Agent session",
      "Dev session",
    ]);
    expect(await screen.findByText("build-box")).toBeTruthy();
    expect(
      screen
        .getByRole("link", { name: "Manage devices in OwnSand" })
        .getAttribute("href"),
    ).toBe(`https://ownsand.delino.io/orgs/${MOCK_IDS.ownsandAcme}/devices`);
  });

  test("reconnects when the WatchQueue stream ends", async () => {
    const mock = createMockTransport({ watchQueueEndsAfterMs: 20 });
    let streams = 0;
    const transport: Transport = {
      ...mock,
      stream: (method, ...rest) => {
        if (method.name === "WatchQueue") streams += 1;
        return mock.stream(method, ...rest);
      },
    };
    renderConsole(`/orgs/${MOCK_IDS.acme}`, { transport });
    await screen.findByRole("table", { name: "Waiting requests" });
    // The SDK waits 1 second before it reconnects.
    await settle(1300);
    expect(streams).toBeGreaterThanOrEqual(2);
    expect(
      screen.getByRole("status", { name: "Queue updates" }).textContent,
    ).toBe("Live");
    expect(
      screen.getByRole("table", { name: "Waiting requests" }),
    ).toBeTruthy();
  });

  test("guides the first setup of a new organization", async () => {
    renderConsole(`/orgs/${MOCK_IDS.globex}`);
    expect(await screen.findByText("Set up tide")).toBeTruthy();
    expect(
      screen
        .getAllByRole("link", { name: "Link OwnSand" })[0]!
        .getAttribute("href"),
    ).toBe(`/orgs/${MOCK_IDS.globex}/integrations`);
  });

  test("an empty list names the next setup step", async () => {
    renderConsole(`/orgs/${MOCK_IDS.globex}/runners`);
    expect(await screen.findByText(/Next step: link ownsand/)).toBeTruthy();
  });
});

describe("work", () => {
  test("canceling a request needs a confirmation", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/requests`);
    const table = await screen.findByRole("table", { name: "Requests" });
    const cancel = within(table).getAllByRole("button", { name: "Cancel" });
    expect(cancel.length).toBe(3);
    await click(cancel[0]!);
    const dialog = await screen.findByRole("alertdialog");
    await click(within(dialog).getByRole("button", { name: "Cancel request" }));
    await settle(80);
    expect(
      within(screen.getByRole("table", { name: "Requests" })).getAllByRole(
        "button",
        { name: "Cancel" },
      ).length,
    ).toBe(2);
  });

  test("dev sessions show only metadata and delete actions by role", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/dev-sessions`);
    const table = await screen.findByRole("table", { name: "Dev sessions" });
    expect(table.textContent).toContain("msvc build");
    expect(
      within(table).getAllByRole("button", { name: "Delete" }).length,
    ).toBe(1);
    expect(document.body.textContent).toContain(
      "shows no commands, output or files",
    );
  });
});

describe("personal API keys", () => {
  test("shows a new secret once with a notice and closes with Escape", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`);
    await screen.findByText(/claude-on-laptop/);
    await click(screen.getByRole("button", { name: "Create API key" }));
    await settle(60);
    const input = screen.getByRole("textbox", { name: "Name" });
    expect(document.activeElement).toBe(input);
    await type(input, "codex-agent");
    await click(screen.getByRole("button", { name: "Create key" }));
    await settle(80);
    const dialog = await screen.findByRole("dialog", {
      name: "Your new personal API key",
    });
    const value = dialog.querySelector("code")!.textContent!;
    expect(value).toMatch(/^tide_[0-9a-f]{48}$/);
    expect(dialog.textContent).toContain("appears only once");
    expect(dialog.textContent).toContain("tide cannot show it again");
    expect(dialog.textContent).toContain("works only for dev sessions");
    expect(within(dialog).getByRole("button", { name: "Copy" })).toBeTruthy();
    expect(storedText()).not.toContain(value);
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByText(value)).toBeNull();
    expect(await screen.findByText(/codex-agent/)).toBeTruthy();
  });
});

describe("personal API key pages", () => {
  test("shows the keys after the first page", async () => {
    const transport = createMockTransport();
    const apiKeys = createClient(ApiKeyService, transport);
    for (let i = 1; i <= PAGE_SIZE; i++) {
      await apiKeys.createApiKey({
        organizationId: MOCK_IDS.acme,
        name: `agent-${i}`,
      });
    }
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByText(/claude-on-laptop/);
    expect(screen.queryByText(new RegExp(`agent-${PAGE_SIZE} `))).toBeNull();
    await click(screen.getByRole("button", { name: "Show more" }));
    await settle(60);
    expect(
      await screen.findByText(new RegExp(`agent-${PAGE_SIZE} `)),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
  });
});

describe("confirmation dialog requests", () => {
  function gatedTransport(methodName: string) {
    const mock = createMockTransport();
    const calls: {
      input: unknown;
      settle(error?: ConnectError): void;
    }[] = [];
    const transport: Transport = {
      ...mock,
      async unary(method, ...args) {
        if (method.name === methodName) {
          const error = await new Promise<ConnectError | undefined>(
            (settle) => {
              calls.push({ input: args[3], settle });
            },
          );
          if (error) throw error;
        }
        return mock.unary(method, ...args);
      },
    };
    return { transport, calls };
  }

  const seededKey = "claude-on-laptop";
  const failure = new ConnectError(
    "Revocation failed",
    Code.FailedPrecondition,
  );

  async function openRevoke(name = seededKey) {
    const row = (await screen.findByText(new RegExp(`^${name} ·`))).closest(
      "li",
    )!;
    await click(within(row).getByRole("button", { name: "Revoke" }));
    return screen.findByRole("alertdialog", { name: `Revoke ${name}?` });
  }

  function confirmButton(dialog: HTMLElement) {
    return within(dialog).getByRole<HTMLButtonElement>("button", {
      name: "Revoke key",
    });
  }

  test("sends one request for two presses and disables confirmation until success", async () => {
    const { transport, calls } = gatedTransport("RevokeApiKey");
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    const button = confirmButton(await openRevoke());
    await act(async () => {
      fireEvent.click(button);
      fireEvent.click(button);
    });
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(button.disabled).toBe(true);

    await act(async () => calls[0].settle());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(calls).toHaveLength(1);
  });

  test("shows a current failure and lets the user retry", async () => {
    const { transport, calls } = gatedTransport("RevokeApiKey");
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    const dialog = await openRevoke();
    const button = confirmButton(dialog);
    await click(button);

    await act(async () => calls[0].settle(failure));
    expect(await within(dialog).findByText("Revocation failed")).toBeTruthy();
    expect(button.disabled).toBe(false);

    await click(button);
    expect(calls).toHaveLength(2);
    expect(button.disabled).toBe(true);
    expect(within(dialog).queryByText("Revocation failed")).toBeNull();
    await act(async () => calls[1].settle());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  test("a late success refreshes the key list and leaves the next confirmation open", async () => {
    const { transport, calls } = gatedTransport("RevokeApiKey");
    const { apiKey } = await createClient(
      ApiKeyService,
      transport,
    ).createApiKey({
      organizationId: MOCK_IDS.acme,
      name: "deploy-bot",
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    const first = await openRevoke();
    await click(confirmButton(first));
    await act(async () => fireEvent.keyDown(first, { key: "Escape" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    const second = await openRevoke("deploy-bot");

    await act(async () => calls[0].settle());
    expect(await screen.findByText(`${seededKey} · Revoked`)).toBeTruthy();
    expect(
      screen.getByRole("alertdialog", { name: "Revoke deploy-bot?" }),
    ).toBe(second);
    expect(confirmButton(second).disabled).toBe(false);
    await click(confirmButton(second));
    expect(calls.map(({ input }) => input)).toEqual([
      expect.objectContaining({ apiKeyId: MOCK_IDS.apiKey }),
      expect.objectContaining({ apiKeyId: apiKey!.apiKeyId }),
    ]);
    await act(async () => calls[1].settle());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  test("a late failure stays hidden after Cancel and reopening the same target", async () => {
    const { transport, calls } = gatedTransport("RevokeApiKey");
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    const first = await openRevoke();
    await click(confirmButton(first));
    await click(within(first).getByRole("button", { name: "Cancel" }));
    const second = await openRevoke();

    await act(async () => calls[0].settle(failure));
    await settle();
    expect(within(second).queryByText("Revocation failed")).toBeNull();
    expect(confirmButton(second).disabled).toBe(false);
  });

  test.each([
    [MOCK_IDS.northwind, "members", "Leave organization", "RemoveMember"],
    [MOCK_IDS.acme, "settings", "Delete organization", "DeleteOrganization"],
  ])(
    "a dismissed %s/%s confirmation does not redirect when %s succeeds",
    async (organizationId, section, label, method) => {
      const { transport, calls } = gatedTransport(method);
      renderConsole(`/orgs/${organizationId}/${section}`, { transport });
      await click(await screen.findByRole("button", { name: label }));
      const dialog = await screen.findByRole("alertdialog");
      await click(within(dialog).getByRole("button", { name: label }));
      await click(within(dialog).getByRole("button", { name: "Cancel" }));
      await click(screen.getByRole("link", { name: "Requests" }));
      const path = `/orgs/${organizationId}/requests`;
      expect(window.location.pathname).toBe(path);

      await act(async () => calls[0].settle());
      await settle();
      expect(window.location.pathname).toBe(path);
    },
  );
});

describe("invitations", () => {
  test("accepts the secret from the link fragment and removes it from the address", async () => {
    renderConsole(`/invitations/accept#secret=${MOCK_INVITATION_SECRET}`);
    await settle(20);
    expect(window.location.hash).toBe("");
    await click(
      await screen.findByRole("button", { name: "Join organization" }),
    );
    await settle(80);
    expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.contoso}`);
    expect(storedText()).not.toContain(MOCK_INVITATION_SECRET);
  });

  test("asks a signed-out user to sign in and open the link again", async () => {
    renderConsole("/invitations/accept", { signedIn: false });
    expect(
      await screen.findByText("Sign in, then open the invitation link again."),
    ).toBeTruthy();
  });
});

describe("settings", () => {
  test("organization deletion tells the owner to uninstall the GitHub App", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
    await click(
      await screen.findByRole("button", { name: "Delete organization" }),
    );
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog.textContent).toContain("Uninstall the tide GitHub App");
  });

  test("an admin changes the dev session limit; the owner actions stay hidden", async () => {
    renderConsole(`/orgs/${MOCK_IDS.globex}/settings`);
    const input = await screen.findByRole("textbox", {
      name: "Limit per user",
    });
    expect(
      screen.queryByRole("button", { name: "Delete organization" }),
    ).toBeNull();
    await type(input, "3");
    await click(screen.getByRole("button", { name: "Save limit" }));
    await settle(60);
    expect(document.body.textContent).not.toContain("Enter a whole number");
  });
});
