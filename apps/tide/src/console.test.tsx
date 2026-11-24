// SPDX-License-Identifier: Apache-2.0
import { create, isMessage } from "@bufbuild/protobuf";
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
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { createQueryOptions } from "@connectrpc/connect-query";
import { ApiKeyService } from "@delino/tide-protos/v1/api_key_pb";
import {
  DeleteAgentIntegrationRequestSchema,
  ListAgentIntegrationsRequestSchema,
  UpdateAgentIntegrationRequestSchema,
} from "@delino/tide-protos/v1/agent_integration_pb";
import { AuditAction, AuditService } from "@delino/tide-protos/v1/audit_pb";
import {
  RequestState,
  Role,
  RunnerState,
} from "@delino/tide-protos/v1/common_pb";
import { GitHubService } from "@delino/tide-protos/v1/github_pb";
import {
  type ListInvitationsRequest,
  type ListMembersRequest,
  MemberService,
  type RevokeInvitationRequest,
} from "@delino/tide-protos/v1/member_pb";
import {
  GetOrganizationResponseSchema,
  OrganizationService,
} from "@delino/tide-protos/v1/organization_pb";
import {
  CreateRunnerProfileRequestSchema,
  type ListRunnerProfilesRequest,
  RunnerProfileService,
} from "@delino/tide-protos/v1/runner_profile_pb";
import {
  type ListRequestsRequest,
  type ListRunnersRequest,
  type WatchQueueResponseSchema,
  WorkService,
} from "@delino/tide-protos/v1/work_pb";
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
    signInFailed: false,
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
    // Use the list structure for bulk checks so the full page fixture does
    // not require computing hundreds of accessible names in happy-dom.
    const rows = list.querySelectorAll("li");
    expect(rows).toHaveLength(201);
    expect(rows[0].textContent).toContain("Acme Robotics");
    expect(rows[199].textContent).toContain("Fixture organization 200");
    expect(rows[200].textContent).toContain("Globex");
    expect(
      list.querySelectorAll(`a[href="/orgs/${MOCK_IDS.globex}"]`),
    ).toHaveLength(1);

    fireEvent.click(
      within(rows[0]).getByRole("link", { name: /Acme Robotics/ }),
    );
    fireEvent.click(
      await screen.findByRole("combobox", { name: "Organization" }),
    );
    fireEvent.click(
      await screen.findByText("Globex", { selector: '[role="option"] *' }),
    );

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

// The full roster renders 200 rows, and its first render can exceed the default timeout in CI.
describe("organization member pagination", () => {
  const laterMemberId = "mock-user-nora";
  const waitForRoster = { timeout: 10_000 };

  async function rosterFixture({
    failToken,
    emptyLastPage = false,
    beforeContinuation,
  }: {
    failToken?: string;
    emptyLastPage?: boolean;
    beforeContinuation?: Promise<void>;
  } = {}) {
    const mock = createMockTransport();
    const memberApi = createClient(MemberService, mock);
    const organizationApi = createClient(OrganizationService, mock);
    const { members } = await memberApi.listMembers({
      organizationId: MOCK_IDS.acme,
      pageSize: 200,
    });
    const laterMember = {
      organizationId: MOCK_IDS.acme,
      userId: laterMemberId,
      role: Role.MEMBER,
    };
    if (!emptyLastPage)
      members.push({ ...members[0]!, ...laterMember, displayName: "" });

    const lists: ListMembersRequest[] = [];
    const updateRole = rs.fn(memberApi.updateMemberRole);
    const remove = rs.fn(memberApi.removeMember);
    const transfer = rs.fn(organizationApi.transferOwnership);
    const roster = createRouterTransport((router) => {
      router.service(MemberService, {
        async listMembers(req) {
          lists.push(req);
          if (req.organizationId !== MOCK_IDS.acme) {
            return memberApi.listMembers(req);
          }
          if (req.pageToken === "p2") await beforeContinuation;
          if (req.pageToken === failToken) {
            failToken = undefined;
            throw new ConnectError("ListMembers failed", Code.Internal);
          }
          return req.pageToken === ""
            ? { members: members.slice(0, 200), nextPageToken: "p2" }
            : {
                // A member may repeat across pages. The first occurrence
                // determines where the row appears.
                members: members.length > 200 ? members.slice(199) : [],
              };
        },
        async updateMemberRole(req) {
          const result = await updateRole(req);
          const member = members.find((member) => member.userId === req.userId);
          if (member) member.role = req.role;
          return result;
        },
        async removeMember(req) {
          const result = await remove(req);
          const index = members.findIndex(
            (member) => member.userId === req.userId,
          );
          if (index !== -1) members.splice(index, 1);
          return result;
        },
      });
      router.service(OrganizationService, {
        transferOwnership: (req) => transfer(req),
      });
    });
    const transport: Transport = {
      ...mock,
      unary(method, ...args) {
        return [
          "ListMembers",
          "UpdateMemberRole",
          "RemoveMember",
          "TransferOwnership",
        ].includes(method.name)
          ? roster.unary(method, ...args)
          : mock.unary(method, ...args);
      },
    };
    return { transport, lists, updateRole, remove, transfer };
  }

  function memberRows() {
    return within(screen.getByRole("list", { name: "Members" })).getAllByRole(
      "listitem",
    );
  }

  function tokens(lists: ListMembersRequest[]) {
    return lists.map(({ pageToken }) => pageToken);
  }

  async function openOwnerPicker() {
    await click(await screen.findByRole("combobox", { name: "New owner" }));
  }

  test.each(["role change", "removal"])(
    "loads a 201st member once and refreshes after %s",
    async (mutation) => {
      const { transport, lists, updateRole, remove } = await rosterFixture();
      renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
      const row = (
        await screen.findByText(laterMemberId, {}, waitForRoster)
      ).closest("li")!;
      expect(memberRows()).toHaveLength(201);
      expect(memberRows()[199]?.textContent).toContain("Roster member 199");
      expect(memberRows()[200]).toBe(row);
      expect(tokens(lists)).toEqual(["", "p2"]);
      expect(lists.every(({ pageSize }) => pageSize === 200)).toBe(true);

      const target = { organizationId: MOCK_IDS.acme, userId: laterMemberId };
      if (mutation === "role change") {
        await click(
          within(row).getByRole("combobox", {
            name: `Role of ${laterMemberId}`,
          }),
        );
        await click(await screen.findByRole("option", { name: "Admin" }));
        await waitFor(() => {
          expect(within(row).getByRole("combobox").textContent).toContain(
            "Admin",
          );
        });
        expect(updateRole).toHaveBeenCalledExactlyOnceWith(
          expect.objectContaining({ ...target, role: Role.ADMIN }),
        );
      } else {
        await click(within(row).getByRole("button", { name: "Remove" }));
        const dialog = await screen.findByRole("alertdialog", {
          name: `Remove ${laterMemberId}?`,
        });
        await click(
          within(dialog).getByRole("button", { name: "Remove member" }),
        );
        await waitFor(() =>
          expect(screen.queryByText(laterMemberId)).toBeNull(),
        );
        expect(remove).toHaveBeenCalledExactlyOnceWith(
          expect.objectContaining(target),
        );
        expect(memberRows()).toHaveLength(200);
      }
      expect(tokens(lists)).toEqual(["", "p2", "", "p2"]);
    },
  );

  test("transfers to a later member and refreshes the former owner's permissions", async () => {
    const { transport, transfer, lists } = await rosterFixture();
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, { transport });
    await waitFor(() => expect(tokens(lists)).toEqual(["", "p2"]));
    await openOwnerPicker();
    expect(screen.queryByRole("option", { name: "Mina Kim" })).toBeNull();
    await click(await screen.findByRole("option", { name: laterMemberId }));
    await click(screen.getByRole("button", { name: "Transfer" }));
    const dialog = await screen.findByRole("alertdialog", {
      name: `Make ${laterMemberId} the owner?`,
    });
    await click(
      within(dialog).getByRole("button", { name: "Transfer ownership" }),
    );

    await waitFor(() =>
      expect(
        screen.queryByRole("heading", { name: "Transfer ownership" }),
      ).toBeNull(),
    );
    expect(transfer).toHaveBeenCalledExactlyOnceWith(
      expect.objectContaining({
        organizationId: MOCK_IDS.acme,
        userId: laterMemberId,
      }),
    );
    expect(tokens(lists)).toEqual(["", "p2", "", "p2"]);
  });

  test("shares loaded pages and retries a failed continuation from either view", async () => {
    let releaseContinuation!: () => void;
    const beforeContinuation = new Promise<void>((resolve) => {
      releaseContinuation = resolve;
    });
    const { transport, lists } = await rosterFixture({
      failToken: "p2",
      beforeContinuation,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
    expect(
      await screen.findByText("Loading more members", {}, waitForRoster),
    ).toBeTruthy();
    expect(memberRows()).toHaveLength(200);
    releaseContinuation();

    expect(
      await screen.findByText(
        "Some members are not shown yet.",
        {},
        waitForRoster,
      ),
    ).toBeTruthy();
    expect(memberRows()).toHaveLength(200);
    await click(screen.getByRole("link", { name: "Settings" }));
    expect(
      await screen.findByText(
        "Some members are not shown yet.",
        {},
        waitForRoster,
      ),
    ).toBeTruthy();
    await openOwnerPicker();
    expect(
      await screen.findByRole("option", { name: "Ada Park" }),
    ).toBeTruthy();
    expect(screen.queryByRole("option", { name: laterMemberId })).toBeNull();
    await act(async () => {
      fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
    });

    await click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() =>
      expect(screen.queryByText("Some members are not shown yet.")).toBeNull(),
    );
    await openOwnerPicker();
    expect(
      await screen.findAllByRole("option", { name: laterMemberId }),
    ).toHaveLength(1);
    expect(tokens(lists)).toEqual(["", "p2", "p2"]);
  });

  test.each(["members", "settings"])(
    "retries a first-page error in %s",
    async (section) => {
      const { transport, lists } = await rosterFixture({
        failToken: "",
        emptyLastPage: true,
      });
      renderConsole(`/orgs/${MOCK_IDS.acme}/${section}`, { transport });
      await click(await screen.findByRole("button", { name: "Try again" }));
      await waitFor(() => expect(tokens(lists)).toEqual(["", "", "p2"]));
      await waitFor(() =>
        expect(screen.queryByText("Loading more members")).toBeNull(),
      );
      if (section === "members") {
        expect(memberRows()).toHaveLength(200);
      } else {
        await openOwnerPicker();
        expect(
          await screen.findByRole("option", { name: "Roster member 199" }),
        ).toBeTruthy();
      }
    },
  );

  test("isolates the roster and transfer permissions when switching organizations", async () => {
    const { transport, lists } = await rosterFixture({ emptyLastPage: true });
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`, { transport });
    await screen.findByText("Roster member 199", {}, waitForRoster);
    await waitFor(() =>
      expect(screen.queryByText("Loading more members")).toBeNull(),
    );
    await click(screen.getByRole("combobox", { name: "Organization" }));
    await click(await screen.findByRole("option", { name: "Globex" }));
    await waitFor(() => expect(memberRows()).toHaveLength(2));
    expect(screen.queryByText("Roster member 199")).toBeNull();
    expect(
      lists.map(({ organizationId, pageToken }) => [organizationId, pageToken]),
    ).toEqual([
      [MOCK_IDS.acme, ""],
      [MOCK_IDS.acme, "p2"],
      [MOCK_IDS.globex, ""],
    ]);
    await click(screen.getByRole("link", { name: "Settings" }));
    expect(
      screen.getByRole("heading", { name: "Dev sessions per user" }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("heading", { name: "Transfer ownership" }),
    ).toBeNull();
    expect(lists).toHaveLength(3);
  });
}, 20_000);

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
  test("retries the failed cursor and scopes loaded integrations to the active organization", async () => {
    const mock = createMockTransport({ agentIntegrationPageSize: 1 });
    const requests: Array<[string, number, string]> = [];
    let failContinuation = true;
    renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`, {
      transport: {
        ...mock,
        async unary(method, signal, timeout, header, input, context) {
          const request = create(method.input, input);
          if (isMessage(request, ListAgentIntegrationsRequestSchema)) {
            requests.push([
              request.organizationId,
              request.pageSize,
              request.pageToken,
            ]);
            if (request.pageToken !== "" && failContinuation) {
              failContinuation = false;
              throw new ConnectError("Continuation failed", Code.Internal);
            }
          }
          return mock.unary(method, signal, timeout, header, input, context);
        },
      },
    });

    const retry = await screen.findByRole("button", { name: "Try again" });
    const list = screen.getByRole("list", { name: "Agent integrations" });
    expect(list.children).toHaveLength(1);
    expect(screen.queryByText("claude-team · Claude Code")).toBeNull();
    await click(retry);
    expect(await screen.findByText("claude-team · Claude Code")).toBeTruthy();
    expect(list.children).toHaveLength(2);
    expect(within(list).getAllByText("codex-prod · OpenAI Codex")).toHaveLength(
      1,
    );
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();

    await click(screen.getByRole("combobox", { name: "Organization" }));
    await click(await screen.findByRole("option", { name: /Northwind Labs/ }));
    expect(
      await screen.findByText("northwind-claude · Claude Code"),
    ).toBeTruthy();
    expect(screen.queryByText("claude-team · Claude Code")).toBeNull();
    expect(requests).toEqual([
      [MOCK_IDS.acme, MAX_PAGE_SIZE, ""],
      [MOCK_IDS.acme, MAX_PAGE_SIZE, "1"],
      [MOCK_IDS.acme, MAX_PAGE_SIZE, "1"],
      [MOCK_IDS.northwind, MAX_PAGE_SIZE, ""],
    ]);
  }, 20_000);

  test.each([Role.ADMIN, Role.MEMBER])(
    "applies the caller role %s to actions for a later-page integration",
    async (role) => {
      const mock = createMockTransport({ agentIntegrationPageSize: 1 });
      const update = rs.fn();
      const remove = rs.fn();
      renderConsole(`/orgs/${MOCK_IDS.acme}/integrations`, {
        transport: {
          ...mock,
          async unary(method, signal, timeout, header, input, context) {
            const request = create(method.input, input);
            if (isMessage(request, UpdateAgentIntegrationRequestSchema)) {
              update(request.organizationId, request.agentIntegrationId);
            }
            if (isMessage(request, DeleteAgentIntegrationRequestSchema)) {
              remove(request.organizationId, request.agentIntegrationId);
            }
            const result = await mock.unary(
              method,
              signal,
              timeout,
              header,
              input,
              context,
            );
            if (
              isMessage(result.message, GetOrganizationResponseSchema) &&
              result.message.organization
            ) {
              result.message.organization.callerRole = role;
            }
            return result;
          },
        },
      });
      const label = await screen.findByText("claude-team · Claude Code");
      const row = within(label.closest("li")!);
      if (role === Role.MEMBER) {
        expect(row.queryByRole("button", { name: "Edit" })).toBeNull();
        expect(row.queryByRole("button", { name: "Delete" })).toBeNull();
        expect(
          screen.queryByRole("button", { name: "Create agent integration" }),
        ).toBeNull();
        return;
      }

      await click(row.getByRole("button", { name: "Edit" }));
      const edit = within(
        await screen.findByRole("dialog", { name: "Edit claude-team" }),
      );
      await type(
        edit.getByRole("textbox", { name: "Name" }),
        "claude-team-renamed",
      );
      await click(edit.getByRole("button", { name: "Save changes" }));
      const renamed = await screen.findByText(
        "claude-team-renamed · Claude Code",
      );
      expect(update).toHaveBeenCalledExactlyOnceWith(
        MOCK_IDS.acme,
        MOCK_IDS.claudeIntegration,
      );

      await click(
        within(renamed.closest("li")!).getByRole("button", { name: "Delete" }),
      );
      const confirmation = within(
        await screen.findByRole("alertdialog", {
          name: "Delete claude-team-renamed?",
        }),
      );
      await click(
        confirmation.getByRole("button", { name: "Delete integration" }),
      );
      expect(remove).toHaveBeenCalledExactlyOnceWith(
        MOCK_IDS.acme,
        MOCK_IDS.claudeIntegration,
      );
    },
    20_000,
  );

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
  test("resolves a later-page integration's name, vendor and ID", async () => {
    const mock = createMockTransport({ agentIntegrationPageSize: 1 });
    const createProfile = rs.fn();
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`, {
      transport: {
        ...mock,
        async unary(method, signal, timeout, header, input, context) {
          const request = create(method.input, input);
          if (isMessage(request, CreateRunnerProfileRequestSchema)) {
            createProfile(
              request.organizationId,
              request.profile?.agentIntegrationId,
            );
          }
          return mock.unary(method, signal, timeout, header, input, context);
        },
      },
    });
    expect(
      await screen.findByText("Integration: claude-team (Claude Code)"),
    ).toBeTruthy();
    await click(screen.getByRole("button", { name: "Create runner profile" }));
    const form = within(
      await screen.findByRole("dialog", { name: "Create a runner profile" }),
    );
    await type(form.getByRole("textbox", { name: "Name" }), "claude-later");
    await click(form.getByRole("combobox", { name: "Workload kind" }));
    await click(await screen.findByRole("option", { name: "Agent session" }));
    await click(form.getByRole("combobox", { name: "Agent integration" }));
    await click(
      await screen.findByRole("option", { name: "claude-team (Claude Code)" }),
    );
    await click(form.getByRole("combobox", { name: "Guest OS" }));
    await click(await screen.findByRole("option", { name: "Windows" }));
    await click(form.getByRole("button", { name: "Create profile" }));
    expect(
      await form.findByText(
        "The vendor of this integration does not run on this guest OS.",
      ),
    ).toBeTruthy();
    expect(createProfile).not.toHaveBeenCalled();

    await click(form.getByRole("combobox", { name: "Guest OS" }));
    await click(await screen.findByRole("option", { name: "Linux" }));
    await click(form.getByRole("button", { name: "Create profile" }));
    expect(
      await screen.findByText("claude-later · Agent session · Linux"),
    ).toBeTruthy();
    expect(createProfile).toHaveBeenCalledExactlyOnceWith(
      MOCK_IDS.acme,
      MOCK_IDS.claudeIntegration,
    );

    const profile = screen
      .getByText("claude-later · Agent session · Linux")
      .closest("li")!;
    await click(within(profile).getByRole("button", { name: "Edit" }));
    const edit = within(
      await screen.findByRole("dialog", { name: "Edit claude-later" }),
    );
    expect(
      edit.getByRole("combobox", { name: "Agent integration" }),
    ).toHaveProperty("disabled", true);
  }, 20_000);

  test.each(["create", "edit"] as const)(
    "keeps the %s dialog open without sending a request for invalid resources",
    async (mode) => {
      const mock = createMockTransport();
      const saveProfile = rs.fn();
      const transport: Transport = {
        ...mock,
        async unary(method, ...args) {
          if (
            method.name ===
              RunnerProfileService.method.createRunnerProfile.name ||
            method.name === RunnerProfileService.method.updateRunnerProfile.name
          ) {
            saveProfile();
          }
          return mock.unary(method, ...args);
        },
      };
      renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`, { transport });

      const creating = mode === "create";
      if (creating) {
        await click(
          await screen.findByRole("button", { name: "Create runner profile" }),
        );
      } else {
        const label = await screen.findByLabelText(
          "runs-on label of linux-build",
        );
        const profile = label.closest("li")!;
        await click(within(profile).getByRole("button", { name: "Edit" }));
      }
      const dialog = await screen.findByRole("dialog", {
        name: creating ? "Create a runner profile" : "Edit linux-build",
      });
      const form = within(dialog);
      if (creating) {
        await type(form.getByRole("textbox", { name: "Name" }), "bounds-test");
      }
      const resources = [
        [
          "vCPUs",
          "3000000000",
          "Enter a whole number of vCPUs from 1 to 1024.",
        ],
        [
          "Memory (MiB)",
          "10000000000000000000",
          "Enter a whole number of MiB from 1 to 16777216.",
        ],
        [
          "Disk (GiB)",
          "1048577",
          "Enter a whole number of GiB from 1 to 1048576.",
        ],
      ] as const;
      for (const [label, value] of resources) {
        const input = form.getByLabelText(label);
        await type(input, value);
        fireEvent.blur(input);
      }
      await click(
        form.getByRole("button", {
          name: creating ? "Create profile" : "Save changes",
        }),
      );

      for (const [label, , message] of resources) {
        expect(form.getByLabelText(label).getAttribute("aria-invalid")).toBe(
          "true",
        );
        expect(form.getByText(message)).toBeTruthy();
      }
      expect(form.queryByText("Something went wrong. Try again.")).toBeNull();
      expect(screen.getByRole("dialog")).toBe(dialog);
      expect(saveProfile).not.toHaveBeenCalled();
    },
  );

  test("follows the token after a full second page to an empty terminal page", async () => {
    const mock = createMockTransport({ runnerProfileCount: 400 });
    const pages: ListRunnerProfilesRequest[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...args) {
        if (
          method.name === RunnerProfileService.method.listRunnerProfiles.name
        ) {
          pages.push(args[3] as ListRunnerProfilesRequest);
        }
        return mock.unary(method, ...args);
      },
    };
    renderConsole(`/orgs/${MOCK_IDS.acme}/profiles`, { transport });

    const list = await screen.findByRole("list", { name: "Runner profiles" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(400);
    expect(rows[0]?.textContent).toContain("codex-linux");
    expect(rows[399]?.textContent).toContain("linux-build");
    expect(
      pages.map(({ organizationId, pageSize, pageToken }) => [
        organizationId,
        pageSize,
        pageToken,
      ]),
    ).toEqual([
      [MOCK_IDS.acme, 200, ""],
      [MOCK_IDS.acme, 200, "200"],
      [MOCK_IDS.acme, 200, "400"],
    ]);
  });

  test("keeps work visible after a continuation fails and retries the full list", async () => {
    const mock = createMockTransport({ runnerProfileCount: 201 });
    const pages: string[] = [];
    const selectedProfiles: string[] = [];
    let fail = true;
    const transport: Transport = {
      ...mock,
      async unary(method, ...args) {
        if (
          method.name === RunnerProfileService.method.listRunnerProfiles.name
        ) {
          const { pageSize, pageToken } = args[3] as ListRunnerProfilesRequest;
          if (pageSize === MAX_PAGE_SIZE) {
            pages.push(pageToken);
            if (pageToken !== "" && fail) {
              fail = false;
              throw new ConnectError("continuation failed", Code.Internal);
            }
          }
        }
        if (
          method.name === WorkService.method.listRequests.name ||
          method.name === WorkService.method.listRunners.name
        ) {
          const { runnerProfileId } = args[3] as
            ListRequestsRequest | ListRunnersRequest;
          if (runnerProfileId) selectedProfiles.push(runnerProfileId);
        }
        return mock.unary(method, ...args);
      },
    };
    renderConsole(`/orgs/${MOCK_IDS.acme}/requests`, {
      transport,
      staleTime: 10_000,
    });

    const error = (
      await screen.findByText(/^Could not load runner profiles/)
    ).closest<HTMLElement>('[role="alert"]')!;
    const requests = await screen.findByRole("table", { name: "Requests" });
    const waitingJob = within(requests).getByRole("row", {
      name: /test \(ubuntu\)/,
    });
    expect(within(waitingJob).queryByText("linux-build")).toBeNull();
    const filter = screen.getByRole<HTMLButtonElement>("combobox", {
      name: "Runner profile",
    });
    expect(filter.disabled).toBe(true);

    await click(within(error).getByRole("button", { name: "Try again" }));
    expect(await within(waitingJob).findByText("linux-build")).toBeTruthy();
    expect(filter.disabled).toBe(false);
    await click(filter);
    await click(await screen.findByRole("option", { name: "linux-build" }));

    await click(screen.getByRole("link", { name: "Runners" }));
    const runners = await screen.findByRole("table", { name: "Runners" });
    expect(within(runners).getAllByText("linux-build")).toHaveLength(2);
    await click(screen.getByRole("combobox", { name: "Runner profile" }));
    await click(await screen.findByRole("option", { name: "linux-build" }));

    await click(screen.getByRole("link", { name: "Overview" }));
    const queue = await screen.findByRole("table", {
      name: "Waiting requests",
    });
    expect(within(queue).getByText("linux-build")).toBeTruthy();
    expect(pages).toEqual(["", "200", "", "200"]);
    expect(selectedProfiles).toEqual([
      MOCK_IDS.linuxProfile,
      MOCK_IDS.linuxProfile,
    ]);
  });

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

  test.each([false, true])(
    "keeps the last snapshot after a terminal stream error (after an automatic reconnect: %s)",
    async (afterReconnect) => {
      let failStream!: () => void;
      const streamFailure = new Promise<void>((resolve) => {
        failStream = resolve;
      });
      let completeSnapshot!: () => void;
      const replacementSnapshot = new Promise<void>((resolve) => {
        completeSnapshot = resolve;
      });
      let streams = 0;
      const queue = createRouterTransport((router) => {
        router.service(WorkService, {
          async *watchQueue(): AsyncGenerator<
            MessageInitShape<typeof WatchQueueResponseSchema>
          > {
            const stream = ++streams;
            if (stream === 1) {
              yield {
                event: {
                  case: "request",
                  value: { requestId: "queued", state: RequestState.QUEUED },
                },
              };
              yield {
                event: {
                  case: "runner",
                  value: { runnerId: "warm", state: RunnerState.WARM },
                },
              };
              yield { event: { case: "snapshotComplete", value: {} } };
              await streamFailure;
              if (afterReconnect) {
                throw new ConnectError("retryable error", Code.Unavailable);
              }
            }
            if (stream === (afterReconnect ? 2 : 1)) {
              throw new ConnectError("internal error", Code.Internal);
            }
            await replacementSnapshot;
            yield { event: { case: "snapshotComplete", value: {} } };
          },
        });
      });
      const mock = createMockTransport();
      const transport: Transport = {
        ...mock,
        stream: (method, ...rest) =>
          (method.name === "WatchQueue" ? queue : mock).stream(method, ...rest),
      };
      renderConsole(`/orgs/${MOCK_IDS.acme}`, { transport });

      const status = within(
        await screen.findByRole("status", { name: "Queue updates" }),
      );
      const warmCount = () =>
        screen.getByText("Warm", { selector: "dt" }).nextElementSibling
          ?.textContent;
      const live = await status.findByText("Live");
      expect(live.getAttribute("data-variant")).toBe("accent");
      expect(screen.getByText("queued")).toBeTruthy();
      expect(warmCount()).toBe("1");

      await act(async () => failStream());
      const disconnected = await status.findByText(
        "Disconnected",
        {},
        { timeout: 3000 },
      );
      expect(disconnected.getAttribute("data-variant")).toBe("neutral");
      expect(screen.getByText("Something went wrong. Try again.")).toBeTruthy();
      const retry = screen.getByRole("button", { name: "Try again" });
      expect(screen.getByText("queued")).toBeTruthy();
      expect(warmCount()).toBe("1");
      expect(streams).toBe(afterReconnect ? 2 : 1);

      await click(retry);
      expect(status.getByText("Reconnecting")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
      expect(screen.getByText("queued")).toBeTruthy();
      expect(warmCount()).toBe("1");
      expect(streams).toBe(afterReconnect ? 3 : 2);

      await act(async () => completeSnapshot());
      expect(status.getByText("Live").getAttribute("data-variant")).toBe(
        "accent",
      );
      expect(screen.getByText("No request is waiting")).toBeTruthy();
      expect(warmCount()).toBe("0");
    },
  );

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

describe("audit", () => {
  function auditActions(table: HTMLElement) {
    return within(table)
      .getAllByRole("row")
      .slice(1)
      .map((row) => within(row).getAllByRole("cell")[1]!.textContent);
  }

  test("shows the seeded audit events newest first", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/audit`);

    const table = await screen.findByRole("table", { name: "Audit events" });
    expect(auditActions(table)).toEqual([
      "Dev session created",
      "OwnSand link set",
    ]);
  });

  test("shows a request cancellation before the seeded audit events", async () => {
    const transport = createMockTransport();
    await createClient(WorkService, transport).cancelRequest({
      organizationId: MOCK_IDS.acme,
      requestId: MOCK_IDS.queuedJob,
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/audit`, { transport });

    const table = await screen.findByRole("table", { name: "Audit events" });
    expect(auditActions(table)).toEqual([
      "Request canceled",
      "Dev session created",
      "OwnSand link set",
    ]);
  });

  test("lists one audit event per page in newest-first order", async () => {
    const client = createClient(AuditService, createMockTransport());
    const organizationId = MOCK_IDS.acme;
    const all = await client.listAuditEvents({ organizationId });
    const first = await client.listAuditEvents({ organizationId, pageSize: 1 });
    expect(first.nextPageToken).not.toBe("");
    const second = await client.listAuditEvents({
      organizationId,
      pageSize: 1,
      pageToken: first.nextPageToken,
    });

    expect(first.events.map((event) => event.action)).toEqual([
      AuditAction.DEV_SESSION_CREATED,
    ]);
    expect(second.events.map((event) => event.action)).toEqual([
      AuditAction.OWNSAND_LINK_SET,
    ]);
    expect([...first.events, ...second.events]).toEqual(all.events);
    expect(second.nextPageToken).toBe("");
  });
});

describe("work", () => {
  test("shows active requests in queue order and can include ended requests", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/requests`);
    const table = await screen.findByRole("table", { name: "Requests" });
    const requestIds = (table: HTMLElement) =>
      within(table)
        .getAllByRole("row")
        .slice(1)
        .map((row) => row.querySelector("code")?.title);
    const active = [
      MOCK_IDS.runningJob,
      MOCK_IDS.queuedJob,
      MOCK_IDS.queuedAgent,
      MOCK_IDS.queuedDev,
    ];
    expect(requestIds(table)).toEqual(active);

    await click(screen.getByRole("checkbox", { name: "Show ended requests" }));

    const updatedTable = await screen.findByRole("table", {
      name: "Requests",
    });
    expect(requestIds(updatedTable)).toEqual([...active, MOCK_IDS.endedJob]);
  });

  test("pages active requests before ended requests in descending end time", async () => {
    const client = createClient(WorkService, createMockTransport());
    const organizationId = MOCK_IDS.acme;
    const time = Date.now();
    const clock = rs.spyOn(Date, "now").mockReturnValue(time);
    try {
      await client.cancelRequest({
        organizationId,
        requestId: MOCK_IDS.queuedJob,
      });
      clock.mockReturnValue(time + 1);
      await client.cancelRequest({
        organizationId,
        requestId: MOCK_IDS.queuedAgent,
      });
    } finally {
      clock.mockRestore();
    }

    const expected = [
      MOCK_IDS.runningJob,
      MOCK_IDS.queuedDev,
      MOCK_IDS.queuedAgent,
      MOCK_IDS.queuedJob,
      MOCK_IDS.endedJob,
    ];
    let pageToken = "";
    for (const [index, requestId] of expected.entries()) {
      const page = await client.listRequests({
        organizationId,
        includeEnded: true,
        pageSize: 1,
        pageToken,
      });
      expect(page.requests.map((request) => request.requestId)).toEqual([
        requestId,
      ]);
      pageToken = page.nextPageToken;
      expect(pageToken === "").toBe(index === expected.length - 1);
    }
  });

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

describe("failed query refreshes", () => {
  const keyPath = `/orgs/${MOCK_IDS.acme}/api-keys`;
  const refreshNotice = /The last refresh failed/;
  const seededKey = /claude-on-laptop/;

  function queryFixture(methodName: string, fixtureNextPage?: string) {
    const mock = createMockTransport();
    const pageTokens: string[] = [];
    let failure: Error | undefined;
    let failContinuationOnly = false;
    const transport: Transport = {
      ...mock,
      async unary(method, ...args) {
        if (method.name !== methodName) return mock.unary(method, ...args);
        const token = (args[3] as { pageToken?: string }).pageToken ?? "";
        pageTokens.push(token);
        if (failure !== undefined && (!failContinuationOnly || token !== "")) {
          throw failure;
        }
        const response = await mock.unary(method, ...args);
        if (fixtureNextPage !== undefined && token === "") {
          Object.assign(response.message, { nextPageToken: fixtureNextPage });
        }
        return response;
      },
    };
    return {
      transport,
      pageTokens,
      fail(error: Error | undefined, continuationOnly = false) {
        failure = error;
        failContinuationOnly = continuationOnly;
      },
    };
  }

  async function refetch(queryClient: QueryClient) {
    await act(async () => {
      await queryClient.refetchQueries({ type: "active" });
    });
  }

  for (const [name, error] of [
    ["unavailable", new ConnectError("offline", Code.Unavailable)],
    ["network error", new TypeError("Failed to fetch")],
    ["internal", new ConnectError("internal", Code.Internal)],
    ["deadline", new ConnectError("deadline", Code.DeadlineExceeded)],
  ] as const) {
    test(`keeps the layout and typed form value after an organization refresh fails with ${name}`, async () => {
      const fixture = queryFixture("GetOrganization");
      const client = renderConsole(keyPath, fixture);
      await screen.findByText(seededKey);
      const nav = screen.getByRole("navigation", { name: "Organization" });
      await click(screen.getByRole("button", { name: "Create API key" }));
      const dialog = screen.getByRole("dialog");
      const input = within(dialog).getByRole<HTMLInputElement>("textbox", {
        name: "Name",
      });
      await type(input, "deploy-bot");

      fixture.fail(error);
      await refetch(client);

      const notice = await screen.findByText(refreshNotice);
      expect(nav.isConnected).toBe(true);
      expect(screen.getByText(seededKey)).toBeTruthy();
      expect(screen.getByRole("dialog")).toBe(dialog);
      expect(within(dialog).getByRole("textbox", { name: "Name" })).toBe(input);
      expect(input.value).toBe("deploy-bot");

      fixture.fail(undefined);
      await click(
        within(notice.closest('[role="alert"]') as HTMLElement).getByRole(
          "button",
          {
            name: "Try again",
            hidden: true,
          },
        ),
      );
      expect(screen.queryByText(refreshNotice)).toBeNull();
      expect(input.value).toBe("deploy-bot");
    });
  }

  test("keeps the one-time secret dialog after an organization refresh fails", async () => {
    const fixture = queryFixture("GetOrganization");
    const client = renderConsole(keyPath, fixture);
    await screen.findByText(seededKey);
    await click(screen.getByRole("button", { name: "Create API key" }));
    await type(screen.getByRole("textbox", { name: "Name" }), "deploy-bot");
    await click(screen.getByRole("button", { name: "Create key" }));
    const dialog = await screen.findByRole("dialog", {
      name: "Your new personal API key",
    });
    const secret = dialog.querySelector("code");
    expect(secret?.textContent).toMatch(/^tide_/);

    fixture.fail(new ConnectError("refresh failed", Code.Internal));
    await refetch(client);

    await screen.findByText(refreshNotice);
    expect(
      screen.getByRole("dialog", { name: "Your new personal API key" }),
    ).toBe(dialog);
    expect(dialog.querySelector("code")).toBe(secret);
  });

  for (const [code, message] of [
    [Code.NotFound, "It was not found, or you no longer have access to it."],
    [Code.PermissionDenied, "You do not have permission to do this."],
    [Code.Unauthenticated, "Your session ended. Sign in again."],
  ] as const) {
    test(`shows a full organization error for ${Code[code]} after loading`, async () => {
      const fixture = queryFixture("GetOrganization");
      const client = renderConsole(keyPath, fixture);
      await screen.findByText(seededKey);

      fixture.fail(new ConnectError("organization unavailable", code));
      await refetch(client);

      expect(await screen.findByText(message)).toBeTruthy();
      if (code === Code.Unauthenticated) {
        expect(
          screen.getByRole("button", { name: "Sign in with Delino" }),
        ).toBeTruthy();
        expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
      } else {
        expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
      }
      expect(
        screen.queryByRole("navigation", { name: "Organization" }),
      ).toBeNull();
      expect(screen.queryByText(seededKey)).toBeNull();
      expect(screen.queryByText(refreshNotice)).toBeNull();
    });
  }

  for (const method of ["GetOrganization", "ListApiKeys"]) {
    test(`shows a full error when the first ${method} request fails`, async () => {
      const fixture = queryFixture(method);
      fixture.fail(new ConnectError("offline", Code.Unavailable));
      renderConsole(keyPath, fixture);

      expect(
        await screen.findByText(/tide is not available right now/),
      ).toBeTruthy();
      expect(screen.queryByText(refreshNotice)).toBeNull();
      expect(screen.queryByText(seededKey)).toBeNull();
      expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
    });
  }

  for (const [path, method, loadedText] of [
    ["/", "ListOrganizations", "Acme Robotics"],
    [`/orgs/${MOCK_IDS.acme}`, "GetCapacity", "build-box"],
    [
      `/orgs/${MOCK_IDS.acme}/profiles`,
      "ListRunnerProfiles",
      "tide-linux-build",
    ],
    [`/orgs/${MOCK_IDS.acme}/members`, "ListMembers", "Ada Park"],
    [
      `/orgs/${MOCK_IDS.acme}/members`,
      "ListInvitations",
      "No pending invitations",
    ],
    [
      `/orgs/${MOCK_IDS.acme}/integrations`,
      "GetOwnSandLink",
      "Manage devices in the OwnSand console",
    ],
    [
      `/orgs/${MOCK_IDS.acme}/integrations`,
      "ListGitHubInstallations",
      "acme-robotics · Active",
    ],
    [
      `/orgs/${MOCK_IDS.acme}/integrations`,
      "ListAgentIntegrations",
      "codex-prod · OpenAI Codex",
    ],
  ] as const) {
    test(`keeps ${method} content after a transient error and replaces it after a terminal error`, async () => {
      const fixture = queryFixture(method);
      const client = renderConsole(path, fixture);
      const content = await screen.findByText(loadedText);

      fixture.fail(new ConnectError("offline", Code.Internal));
      await refetch(client);

      await screen.findByText(refreshNotice);
      expect(screen.getByText(loadedText)).toBe(content);

      fixture.fail(new ConnectError("access revoked", Code.PermissionDenied));
      await refetch(client);

      await screen.findByText(/You do not have permission to do this\.$/);
      expect(screen.queryByText(loadedText)).toBeNull();
      expect(screen.queryByText(refreshNotice)).toBeNull();
    });
  }

  test("keeps the selected runner group after a refresh fails inside the dialog", async () => {
    const fixture = queryFixture("ListGitHubRunnerGroups");
    const client = renderConsole(
      `/orgs/${MOCK_IDS.acme}/integrations`,
      fixture,
    );
    await click(
      await screen.findByRole("button", { name: "Change runner group" }),
    );
    const dialog = screen.getByRole("dialog");
    const group = await within(dialog).findByRole("combobox", {
      name: "Runner group",
    });

    fixture.fail(new ConnectError("offline", Code.Internal));
    await refetch(client);

    await within(dialog).findByText(refreshNotice);
    expect(within(dialog).getByRole("combobox", { name: "Runner group" })).toBe(
      group,
    );
    fixture.fail(undefined);
    await click(within(dialog).getByRole("button", { name: "Try again" }));
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect(within(dialog).queryByText(refreshNotice)).toBeNull();
  });

  const infiniteLists = [
    ["api-keys", "ListApiKeys", "list", "Personal API keys"],
    ["audit", "ListAuditEvents", "table", "Audit events"],
    ["requests", "ListRequests", "table", "Requests"],
    ["runners", "ListRunners", "table", "Runners"],
    ["dev-sessions", "ListDevSessions", "table", "Dev sessions"],
  ] as const;

  for (const [route, method, role, name] of infiniteLists) {
    test(`keeps ${route} loaded when a background refresh fails`, async () => {
      const fixture = queryFixture(method);
      const client = renderConsole(`/orgs/${MOCK_IDS.acme}/${route}`, fixture);
      const list = await screen.findByRole(role, { name });

      fixture.fail(new ConnectError("offline", Code.Internal));
      await refetch(client);

      await screen.findByText(refreshNotice);
      expect(screen.getByRole(role, { name })).toBe(list);
      fixture.fail(undefined);
      await click(screen.getByRole("button", { name: "Try again" }));
      expect(screen.queryByText(refreshNotice)).toBeNull();
      expect(screen.getByRole(role, { name })).toBe(list);
    });

    test(`retries the failed ${route} continuation with its original page token`, async () => {
      const fixture = queryFixture(method, "50");
      renderConsole(`/orgs/${MOCK_IDS.acme}/${route}`, fixture);
      const list = await screen.findByRole(role, { name });

      fixture.fail(
        new ConnectError("continuation failed", Code.Internal),
        true,
      );
      await click(screen.getByRole("button", { name: "Show more" }));
      expect(
        await screen.findByText("Something went wrong. Try again."),
      ).toBeTruthy();
      expect(screen.queryByText(refreshNotice)).toBeNull();
      expect(screen.getByRole(role, { name })).toBe(list);

      fixture.fail(undefined);
      await click(screen.getByRole("button", { name: "Try again" }));
      expect(fixture.pageTokens).toEqual(["", "50", "50"]);
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
      expect(screen.getByRole(role, { name })).toBe(list);
    });
  }

  test("replaces an infinite list after a terminal continuation error", async () => {
    const fixture = queryFixture("ListApiKeys", "50");
    renderConsole(keyPath, fixture);
    await screen.findByText(seededKey);

    fixture.fail(
      new ConnectError("access revoked", Code.PermissionDenied),
      true,
    );
    await click(screen.getByRole("button", { name: "Show more" }));

    expect(
      await screen.findByText("You do not have permission to do this."),
    ).toBeTruthy();
    expect(screen.queryByText(seededKey)).toBeNull();
    expect(screen.queryByText(refreshNotice)).toBeNull();
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
    fixture.fail(undefined);
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(fixture.pageTokens).toEqual(["", "50", ""]);
    expect(await screen.findByText(seededKey)).toBeTruthy();
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
  function invitationFixture({ failContinuation = false } = {}) {
    const mock = createMockTransport();
    const client = createClient(MemberService, mock);
    const listRequests: ListInvitationsRequest[] = [];
    const revokeRequests: RevokeInvitationRequest[] = [];
    const fixture = createRouterTransport((router) => {
      router.service(MemberService, {
        async listInvitations(req, { signal }) {
          listRequests.push(req);
          if (failContinuation && req.pageToken !== "") {
            failContinuation = false;
            throw new ConnectError("Continuation failed", Code.Internal);
          }
          const page = await client.listInvitations(req, { signal });
          // The control plane returns a token at the end of every full page.
          if (page.invitations.length === req.pageSize && !page.nextPageToken) {
            page.nextPageToken = String(Number(req.pageToken) + req.pageSize);
          }
          return page;
        },
        revokeInvitation(req, { signal }) {
          revokeRequests.push(req);
          return client.revokeInvitation(req, { signal });
        },
      });
    });
    const transport: Transport = {
      ...mock,
      unary(method, ...args) {
        const handler =
          method.name === "ListInvitations" ||
          method.name === "RevokeInvitation"
            ? fixture
            : mock;
        return handler.unary(method, ...args);
      },
    };
    return { client, listRequests, revokeRequests, transport };
  }

  test("lists and revokes a later-page invitation and refreshes after writes", async () => {
    const { client, listRequests, revokeRequests, transport } =
      invitationFixture();
    const organizationId = MOCK_IDS.globex;
    const invite = { organizationId, role: Role.MEMBER };
    const seeded = [];
    for (let i = 0; i <= MAX_PAGE_SIZE; i++) {
      seeded.push(await client.createInvitation(invite));
    }
    renderConsole(`/orgs/${organizationId}/members`, { transport });
    const list = await screen.findByRole("list", {
      name: "Pending invitations",
    });
    expect(within(list).getAllByRole("listitem").length).toBe(201);
    expect(
      [...list.querySelectorAll("code")].map((code) => code.textContent),
    ).toEqual(seeded.map(({ invitation }) => `${invitation!.secretPrefix}…`));

    const revoke = within(list).getAllByRole("button", { name: "Revoke" });
    await click(revoke.at(-1)!);
    expect(revokeRequests).toEqual([
      expect.objectContaining({
        organizationId,
        invitationId: seeded.at(-1)!.invitation!.invitationId,
      }),
    ]);
    await waitFor(() =>
      expect(
        within(
          screen.getByRole("list", { name: "Pending invitations" }),
        ).getAllByRole("listitem"),
      ).toHaveLength(200),
    );

    await click(screen.getByRole("button", { name: "Invite someone" }));
    await click(screen.getByRole("button", { name: "Create link" }));
    const dialog = await screen.findByRole("dialog", {
      name: "Your invitation link",
    });
    expect(dialog.querySelector("code")?.textContent).toContain(
      "/invitations/accept#secret=tdi_",
    );
    await click(within(dialog).getByRole("button", { name: "Done" }));
    expect(
      screen.queryByRole("dialog", { name: "Your invitation link" }),
    ).toBeNull();
    expect(within(list).getAllByRole("listitem").length).toBe(201);
    expect(
      listRequests.map(({ organizationId, pageToken, pageSize }) => ({
        organizationId,
        pageToken,
        pageSize,
      })),
    ).toEqual(
      ["", "200", "", "200", "", "200"].map((pageToken) => ({
        organizationId,
        pageToken,
        pageSize: MAX_PAGE_SIZE,
      })),
    );
  });

  test("retries the complete list after a continuation error and stops at an empty terminal page", async () => {
    const { client, listRequests, transport } = invitationFixture({
      failContinuation: true,
    });
    const organizationId = MOCK_IDS.acme;
    for (let i = 0; i < MAX_PAGE_SIZE; i++) {
      await client.createInvitation({ organizationId, role: Role.MEMBER });
    }
    renderConsole(`/orgs/${organizationId}/members`, { transport });
    const retry = await screen.findByRole("button", { name: "Try again" });
    expect(screen.queryByText("No pending invitations")).toBeNull();
    expect(
      screen.queryByRole("list", { name: "Pending invitations" }),
    ).toBeNull();
    await click(retry);
    const list = await screen.findByRole("list", {
      name: "Pending invitations",
    });
    expect(within(list).getAllByRole("listitem").length).toBe(200);
    expect(listRequests.map(({ pageToken }) => pageToken)).toEqual([
      "",
      "200",
      "",
      "200",
    ]);
  });

  function holdInvitationAccept() {
    const mock = createMockTransport();
    let release = () => {};
    const promise = new Promise<void>((resolve) => {
      release = resolve;
    });
    const requested = rs.fn();
    const transport: Transport = {
      ...mock,
      async unary(method, ...args) {
        if (method.name === MemberService.method.acceptInvitation.name) {
          requested();
          await promise;
        }
        return mock.unary(method, ...args);
      },
    };
    return { transport, release, requested };
  }

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

  test.each(["the tide link", "browser Back"])(
    "refreshes organizations without navigating after the user leaves via %s",
    async (departure) => {
      const { transport, release, requested } = holdInvitationAccept();
      renderConsole(`/invitations/accept#secret=${MOCK_INVITATION_SECRET}`, {
        transport,
      });
      await click(screen.getByRole("button", { name: "Join organization" }));
      expect(requested).toHaveBeenCalledTimes(1);

      if (departure === "the tide link") {
        await click(screen.getByRole("link", { name: "tide" }));
        await click(await screen.findByRole("link", { name: /Acme Robotics/ }));
      } else {
        await act(async () => {
          window.history.replaceState(null, "", `/orgs/${MOCK_IDS.acme}`);
          window.dispatchEvent(new PopStateEvent("popstate"));
        });
      }
      const switcher = await screen.findByRole("combobox", {
        name: "Organization",
      });
      await click(switcher);
      expect(screen.queryByRole("option", { name: /Contoso/ })).toBeNull();

      await act(async () => release());

      expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.acme}`);
      expect(
        await screen.findByRole("option", { name: /Contoso/ }),
      ).toBeTruthy();
    },
  );

  test("keeps the new invitation when an earlier accept succeeds", async () => {
    const { transport, release, requested } = holdInvitationAccept();
    renderConsole(`/invitations/accept#secret=${MOCK_INVITATION_SECRET}`, {
      transport,
    });
    const join = screen.getByRole<HTMLButtonElement>("button", {
      name: "Join organization",
    });
    await click(join);
    expect(requested).toHaveBeenCalledTimes(1);

    const input = screen.getByLabelText<HTMLInputElement>(
      "Invitation link or secret",
    );
    await type(input, "tdi_follow_up_invitation");
    await act(async () => release());
    await waitFor(() => expect(join.disabled).toBe(false));

    expect(window.location.pathname).toBe("/invitations/accept");
    expect(input.value).toBe("tdi_follow_up_invitation");
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
