// SPDX-License-Identifier: Apache-2.0
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
  within,
} from "@testing-library/react";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { ApiKeyService } from "@delino/tide-protos/v1/api_key_pb";
import { QueryClient } from "@tanstack/react-query";
import { PAGE_SIZE } from "./api.ts";
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
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
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
  test("strips code and state, links the installation and continues to integrations", async () => {
    renderConsole(
      "/github/setup?installation_id=51000001&code=gh_code&state=mock-state&setup_action=install",
    );
    await settle(20);
    expect(window.location.search).toBe("");
    expect(window.location.href).not.toContain("gh_code");
    // The user is an admin of Acme and Globex, so the page asks which one.
    const form = await screen.findByRole("button", {
      name: "Link installation",
    });
    expect(form).toBeTruthy();
    const trigger = screen.getByRole("combobox", { name: "tide organization" });
    await click(trigger);
    await click(await screen.findByRole("option", { name: /Globex/ }));
    await click(screen.getByRole("button", { name: "Link installation" }));
    await settle(80);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.globex}/integrations`,
    );
    expect(await screen.findByText(/github-org-51000001/)).toBeTruthy();
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
