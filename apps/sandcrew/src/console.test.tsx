// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { Role } from "@delino/sandcrew-protos/v1/common_pb";
import { OrganizationSchema } from "@delino/sandcrew-protos/v1/organization_pb";
import {
  TranscriptEventSchema,
  type WatchSessionResponse,
} from "@delino/sandcrew-protos/v1/session_pb";
import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { Console } from "./App.tsx";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { OrgContextProvider } from "./components/shell.tsx";
import { AuthMode } from "./config.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
  type MockTransportOptions,
  SESSION_LIMIT_MESSAGE,
} from "./mock/mock-api.ts";
import { TranscriptView } from "./pages/session-chat.tsx";
import { UserSettingsPage } from "./pages/settings-user.tsx";
import { RouterProvider } from "./router.tsx";
import { buildTranscript } from "./transcript.ts";
import type { PushEnvironment } from "./web-push.ts";

const config = {
  apiOrigin: "https://api.test",
  authMode: AuthMode.Mock,
  logto: undefined,
};

const ACME = MOCK_IDS.acme;

function auth(signedIn = true): Auth {
  return {
    isLoading: false,
    isAuthenticated: signedIn,
    userId: signedIn ? MOCK_USER : undefined,
    signIn: async () => {},
    signInFailed: false,
    signOut: async () => {},
    getAccessToken: async () => "token",
  };
}

function testQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
}

/** Records the RPC names that a transport received. */
function recording(transport: Transport) {
  const calls: { method: string; input: unknown }[] = [];
  const recorded: Transport = {
    ...transport,
    async unary(method, signal, timeout, header, input, contextValues) {
      calls.push({ method: method.name, input });
      return transport.unary(
        method,
        signal,
        timeout,
        header,
        input,
        contextValues,
      );
    },
  };
  return { transport: recorded, calls };
}

function renderConsole(
  path: string,
  options: {
    mock?: MockTransportOptions;
    transport?: Transport;
    signedIn?: boolean;
  } = {},
) {
  window.history.replaceState(null, "", path);
  const transport =
    options.transport ??
    createMockTransport({ agentStepMs: 5, ...options.mock });
  render(
    <AuthContextProvider value={auth(options.signedIn)}>
      <Console
        config={config}
        transport={transport}
        queryClient={testQueryClient()}
      />
    </AuthContextProvider>,
  );
  return transport;
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

async function choose(combobox: HTMLElement, option: string) {
  await click(combobox);
  await click(await screen.findByRole("option", { name: option }));
}

/** Everything that the console keeps in browser storage. */
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

describe("sign-in", () => {
  test("asks a signed-out user to sign in", async () => {
    renderConsole("/", { signedIn: false });
    expect(
      await screen.findByRole("button", { name: "Sign in with Delino" }),
    ).toBeTruthy();
  });
});

describe("organization picker", () => {
  test("lists the organizations with the caller's role and links to SandRun", async () => {
    renderConsole("/");
    const list = await screen.findByRole("list", { name: "Organizations" });
    expect(within(list).getByText("Acme Robotics")).toBeTruthy();
    expect(within(list).getByText("Member")).toBeTruthy();
    const sandrun = screen.getAllByRole("link", {
      name: "SandRun console",
    })[0]!;
    expect(sandrun.getAttribute("href")).toBe("https://sandrun.delino.io");
    expect(sandrun.getAttribute("rel")).toBe("noopener noreferrer");
  });

  test("shows an empty state that links to the SandRun console", async () => {
    const mock = createMockTransport();
    const transport: Transport = {
      ...mock,
      async unary(method, ...args) {
        if (method.name === "ListOrganizations") {
          return {
            method,
            header: new Headers(),
            trailer: new Headers(),
            stream: false,
            service: method.parent,
            message: {
              $typeName: "delino.sandcrew.v1.ListOrganizationsResponse",
              organizations: [],
              nextPageToken: "",
            },
          } as never;
        }
        return mock.unary(method, ...args);
      },
    };
    renderConsole("/", { transport });
    expect(
      await screen.findByText("You are not in an organization yet"),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Open the SandRun console" }),
    ).toBeTruthy();
  });

  test("shows an error with a retry", async () => {
    renderConsole("/", { mock: { failOrganizations: Code.Unavailable } });
    expect(
      await screen.findByText(
        "SandCrew is not available right now. Try again in a moment.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });

  test("returns to the picker from an organization the user does not belong to", async () => {
    renderConsole("/orgs/0194c000-0000-7000-8000-0000000000ff/sessions");
    expect(
      await screen.findByText(
        "You no longer have access to that organization. Select another one.",
      ),
    ).toBeTruthy();
    expect(window.location.pathname).toBe("/");
    expect(window.location.search).toBe("");
  });
});

describe("organization shell", () => {
  test("hides the organization settings from members", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/settings/organization`);
    expect(
      await screen.findByText(
        "Only owners and admins of the organization change its SandCrew settings.",
      ),
    ).toBeTruthy();
    const nav = screen.getByRole("navigation", { name: "Organization" });
    expect(
      within(nav).queryByRole("link", { name: "Organization settings" }),
    ).toBeNull();
  });

  test("shows the organization settings to owners", async () => {
    renderConsole(`/orgs/${ACME}/sessions`);
    const nav = await screen.findByRole("navigation", { name: "Organization" });
    expect(
      within(nav).getByRole("link", { name: "Organization settings" }),
    ).toBeTruthy();
    expect(
      within(nav)
        .getByRole("link", { name: "Sessions" })
        .getAttribute("aria-current"),
    ).toBe("page");
    expect(window.localStorage.getItem("sandcrew.selected-organization")).toBe(
      ACME,
    );
  });
});

describe("session list", () => {
  test("lists the caller's sessions with states and end reasons", async () => {
    renderConsole(`/orgs/${ACME}/sessions`);
    const list = await screen.findByRole("list", { name: "Sessions" });
    expect(within(list).getByText("Fix the flaky upload test")).toBeTruthy();
    expect(within(list).getByText("Waiting for input")).toBeTruthy();
    expect(within(list).getByText("Paused")).toBeTruthy();
    expect(within(list).getByText("Ended: The agent exited")).toBeTruthy();
    expect(within(list).getByText("Disconnected")).toBeTruthy();
    expect(
      within(list).getByText(/^Deleted on .* unless it resumes$/),
    ).toBeTruthy();
    expect(within(list).queryByText("Refactor the billing module")).toBeNull();
  });

  test("shows other members' sessions with All sessions and remembers the filter", async () => {
    renderConsole(`/orgs/${ACME}/sessions`);
    await screen.findByRole("list", { name: "Sessions" });
    await click(screen.getByRole("radio", { name: "All sessions" }));
    const list = await screen.findByRole("list", { name: "Sessions" });
    await waitFor(() =>
      expect(
        within(list).getByText("Refactor the billing module"),
      ).toBeTruthy(),
    );
    expect(within(list).getByText(/Another member/)).toBeTruthy();
    expect(window.localStorage.getItem("sandcrew.session-filter")).toBe("all");
  });

  test("reloads the list after its change stream reconnects", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    const { transport: recorded, calls } = recording(base);
    let watches = 0;
    const transport: Transport = {
      ...recorded,
      async stream(method, ...rest) {
        const response = await base.stream(method, ...rest);
        if (method.name !== "WatchSessions" || ++watches > 1) return response;
        // The first stream fails after its first response.
        return {
          ...response,
          message: (async function* () {
            for await (const message of response.message) {
              yield message;
              throw new ConnectError("connection reset", Code.Unavailable);
            }
          })(),
        };
      },
    };
    const lists = () => calls.filter((c) => c.method === "ListSessions").length;
    renderConsole(`/orgs/${ACME}/sessions`, { transport });
    await screen.findByRole("list", { name: "Sessions" });
    const before = lists();
    await waitFor(() => expect(watches).toBe(2), { timeout: 3000 });
    await waitFor(() => expect(lists()).toBeGreaterThan(before), {
      timeout: 3000,
    });
  });

  test("backs off while its change stream fails before a response", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    let watches = 0;
    const transport: Transport = {
      ...base,
      async stream(method, ...rest) {
        if (method.name !== "WatchSessions")
          return base.stream(method, ...rest);
        watches++;
        throw new ConnectError("unavailable", Code.Unavailable);
      },
    };
    renderConsole(`/orgs/${ACME}/sessions`, { transport });
    await screen.findByRole("list", { name: "Sessions" });
    await waitFor(() => expect(watches).toBe(1));
    // Attempts at 0, 1 and 3 seconds; without backoff, one every second.
    await new Promise((resolve) => setTimeout(resolve, 4_500));
    expect(watches).toBe(3);
  }, 10_000);

  test("keeps a failed first load recoverable", async () => {
    renderConsole(`/orgs/${ACME}/sessions`, {
      mock: { failFirstSessionList: true },
    });
    await click(await screen.findByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("list", { name: "Sessions" })).toBeTruthy();
  });

  test("shows an empty state", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/sessions`);
    expect(await screen.findByText("You have no sessions yet")).toBeTruthy();
  });
});

describe("new-session form", () => {
  test("stays on another page when a session starts after the user left", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const { transport, calls } = recording({
      ...base,
      async unary(method, ...rest) {
        if (method.name === "CreateSession") await held;
        return base.unary(method, ...rest);
      },
    });
    renderConsole(`/orgs/${ACME}/sessions/new`, { transport });
    await type(
      await screen.findByRole("textbox", { name: "First message" }),
      "Clone the repository",
    );
    await click(screen.getByRole("button", { name: "Start session" }));
    await click(screen.getByRole("link", { name: "Settings" }));
    const settingsPath = window.location.pathname;
    expect(settingsPath).toBe(`/orgs/${ACME}/settings`);
    await act(async () => release());
    await waitFor(() =>
      expect(calls.some((c) => c.method === "CreateSession")).toBe(true),
    );
    await settle();
    expect(window.location.pathname).toBe(settingsPath);
  });

  test("shows a credential load failure and recovers on retry", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    let failures = 1;
    const transport: Transport = {
      ...base,
      async unary(method, ...rest) {
        if (method.name === "ListVendorCredentials" && failures > 0) {
          failures--;
          throw new ConnectError("unavailable", Code.Unavailable);
        }
        return base.unary(method, ...rest);
      },
    };
    renderConsole(`/orgs/${ACME}/sessions/new`, { transport });
    await screen.findByRole("textbox", { name: "First message" });
    await click(await screen.findByRole("button", { name: "Try again" }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull(),
    );
    await type(
      screen.getByRole("textbox", { name: "First message" }),
      "Clone the repository",
    );
    expect(
      (
        screen.getByRole("button", {
          name: "Start session",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  });

  test("focuses the first message and starts a session with the selected secret", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/sessions/new`, { transport });
    const message = await screen.findByRole("textbox", {
      name: "First message",
    });
    await waitFor(() => expect(document.activeElement).toBe(message));
    expect(screen.queryByText(/You have no Claude Code credential/)).toBeNull();
    await type(message, "Clone the repository");
    await type(
      screen.getByRole("textbox", { name: "Label (optional)" }),
      "Clone",
    );
    const secret = await screen.findByRole("checkbox", {
      name: "GITHUB_TOKEN",
    });
    expect(secret.getAttribute("aria-checked")).toBe("false");
    await click(secret);
    await choose(screen.getByRole("combobox", { name: "Model" }), "Opus");
    await click(screen.getByRole("button", { name: "Start session" }));

    await waitFor(() =>
      expect(window.location.pathname).toMatch(
        new RegExp(`^/orgs/${ACME}/sessions/[0-9a-f-]{36}$`),
      ),
    );
    const create = calls.find((c) => c.method === "CreateSession")!.input as {
      firstMessage: string;
      label: string;
      sessionSecretIds: string[];
      configOptions: { configOptionId: string; valueId: string }[];
    };
    expect(create.firstMessage).toBe("Clone the repository");
    expect(create.label).toBe("Clone");
    expect(create.sessionSecretIds).toHaveLength(1);
    expect(create.configOptions).toEqual([
      expect.objectContaining({ configOptionId: "model", valueId: "opus" }),
    ]);
    expect(
      await screen.findByText("I read your message: Clone the repository"),
    ).toBeTruthy();
  });

  test("explains a missing vendor credential", async () => {
    renderConsole(`/orgs/${ACME}/sessions/new`);
    await choose(
      await screen.findByRole("combobox", { name: "Agent" }),
      "OpenAI Codex",
    );
    expect(
      await screen.findByText(/You have no OpenAI Codex credential/),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Add a credential in your settings" }),
    ).toBeTruthy();
    expect(
      (
        screen.getByRole("button", {
          name: "Start session",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
  });

  test("asks members to contact an administrator without an allowed profile", async () => {
    renderConsole(`/orgs/${MOCK_IDS.northwind}/sessions/new`);
    expect(
      await screen.findByText(
        /Ask an owner or admin of the organization to allow a runner profile/,
      ),
    ).toBeTruthy();
    expect(
      screen.queryByRole("link", { name: "Go to the organization settings" }),
    ).toBeNull();
  });

  test("shows the SandRun session limit message", async () => {
    renderConsole(`/orgs/${MOCK_IDS.globex}/sessions/new`);
    await type(
      await screen.findByRole("textbox", { name: "First message" }),
      "Hello",
    );
    await click(screen.getByRole("button", { name: "Start session" }));
    expect(await screen.findByText(SESSION_LIMIT_MESSAGE)).toBeTruthy();
  });

  test("requires the first message", async () => {
    renderConsole(`/orgs/${ACME}/sessions/new`);
    await screen.findByRole("textbox", { name: "First message" });
    await click(screen.getByRole("button", { name: "Start session" }));
    expect(await screen.findByText("Enter the first message.")).toBeTruthy();
  });
});

describe("chat view", () => {
  const chatPath = `/orgs/${ACME}/sessions/${MOCK_IDS.activeSession}`;

  test("reloads the session after its stream reconnects", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    const { transport: recorded, calls } = recording(base);
    let watches = 0;
    const transport: Transport = {
      ...recorded,
      async stream(method, ...rest) {
        const response = await base.stream(method, ...rest);
        if (method.name !== "WatchSession" || ++watches > 1) return response;
        // The first stream fails after it caught up.
        return {
          ...response,
          message: (async function* () {
            for await (const message of response.message) {
              yield message;
              if ((message as WatchSessionResponse).event.case === "caughtUp") {
                throw new ConnectError("connection reset", Code.Unavailable);
              }
            }
          })(),
        };
      },
    };
    const gets = () => calls.filter((c) => c.method === "GetSession").length;
    renderConsole(chatPath, { transport });
    await screen.findByRole("region", { name: "Transcript" });
    await waitFor(() => expect(watches).toBe(1));
    const before = gets();
    await waitFor(() => expect(watches).toBe(2), { timeout: 3000 });
    await waitFor(() => expect(gets()).toBeGreaterThan(before), {
      timeout: 3000,
    });
  });

  test("shows every kind of transcript event", async () => {
    renderConsole(chatPath);
    const transcript = await screen.findByRole("region", {
      name: "Transcript",
    });
    const firstMessage = within(transcript).getAllByRole("article", {
      name: "Your message",
    })[0]!;
    expect(firstMessage.textContent).toContain(
      "Clone https://github.com/example/uploader",
    );
    // GitHub Flavored Markdown links a bare URL.
    expect(
      within(firstMessage)
        .getByRole("link", { name: "https://github.com/example/uploader" })
        .getAttribute("rel"),
    ).toBe("noopener noreferrer");
    // Thoughts start collapsed.
    expect(
      within(transcript)
        .getByRole("button", { name: "Thought" })
        .getAttribute("aria-expanded"),
    ).toBe("false");
    expect(
      within(transcript).getByRole("article", { name: "Plan" }),
    ).toBeTruthy();
    expect(within(transcript).getByText("fixed").tagName).toBe("STRONG");
    const link = within(transcript).getByRole("link", { name: "the docs" });
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(
      within(transcript).getByLabelText("Changes in uploader/upload_test.go")
        .textContent,
    ).toContain("+ \twaitForRetry(t)");
    expect(within(transcript).getByText("Auto-approved")).toBeTruthy();
    expect(
      within(transcript).getByText(/The agent rejected an option/),
    ).toBeTruthy();
    expect(
      within(transcript).getByRole("button", {
        name: "Agent update: usage_update",
      }),
    ).toBeTruthy();
    expect(within(transcript).getByText("Turn ended")).toBeTruthy();
    expect(within(transcript).getByText(/Ask before tools run/)).toBeTruthy();
    const tool = within(transcript).getByRole("article", {
      name: "Tool call: go test ./...",
    });
    expect(within(tool).getByText("Pending")).toBeTruthy();
  });

  test("shows raw agent JSON as text inside a collapsed section", async () => {
    renderConsole(chatPath);
    const transcript = await screen.findByRole("region", {
      name: "Transcript",
    });
    const toggle = within(transcript).getByRole("button", {
      name: "Agent update: usage_update",
    });
    await click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(within(transcript).getByText(/"used": 1200/)).toBeTruthy();
  });

  test("answers a permission request with one button per option", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(chatPath, { transport });
    const request = await screen.findByRole("article", {
      name: "Permission request for go test ./...",
    });
    expect(
      within(request)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Allow once", "Always allow", "Reject"]);
    await click(within(request).getByRole("button", { name: "Allow once" }));
    expect(
      calls.find((c) => c.method === "AnswerPermission")?.input,
    ).toMatchObject({
      permissionRequestId: "perm-2",
      optionId: "allow",
    });
    await waitFor(() =>
      expect(
        within(request).queryByRole("button", { name: "Allow once" }),
      ).toBeNull(),
    );
    expect(await screen.findByText("The command finished.")).toBeTruthy();
  });

  test("sends a message, shows the reply and stops a running turn", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 200 }),
    );
    renderConsole(chatPath, { transport });
    const message = await screen.findByRole("textbox", { name: "Message" });
    await type(message, "Explain the fix");
    await click(screen.getByRole("button", { name: "Send" }));
    expect(calls.find((c) => c.method === "SendMessage")?.input).toMatchObject({
      text: "Explain the fix",
    });
    const stop = await screen.findByRole("button", { name: "Stop" });
    await click(stop);
    expect(calls.some((c) => c.method === "CancelTurn")).toBe(true);
    expect(await screen.findByText("Turn stopped")).toBeTruthy();
  });

  test("reuses the message ID when a failed send is retried", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    const sent: { clientMessageId: string; text: string }[] = [];
    let loseResponse = true;
    const transport: Transport = {
      ...base,
      async unary(method, signal, timeout, header, input, contextValues) {
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        if (method.name === "SendMessage") {
          sent.push(input as { clientMessageId: string; text: string });
          // The server accepted the first message, but its response is lost.
          if (loseResponse) {
            loseResponse = false;
            throw new ConnectError("connection reset", Code.Unavailable);
          }
        }
        return response;
      },
    };
    renderConsole(chatPath, { transport });
    const message = await screen.findByRole("textbox", { name: "Message" });
    await type(message, "Explain the fix");
    await click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect((message as HTMLTextAreaElement).value).toBe("Explain the fix");
    await click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(sent).toHaveLength(2));
    expect(sent[1]!.clientMessageId).toBe(sent[0]!.clientMessageId);
    await waitFor(() =>
      expect((message as HTMLTextAreaElement).value).toBe(""),
    );
    await type(message, "Explain the fix");
    await click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(sent).toHaveLength(3));
    expect(sent[2]!.clientMessageId).not.toBe(sent[0]!.clientMessageId);
  });

  test("keeps a draft typed while a message is sending", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const transport: Transport = {
      ...base,
      async unary(method, ...rest) {
        if (method.name === "SendMessage") await held;
        return base.unary(method, ...rest);
      },
    };
    renderConsole(chatPath, { transport });
    const message = await screen.findByRole("textbox", { name: "Message" });
    await type(message, "Explain the fix");
    await click(screen.getByRole("button", { name: "Send" }));
    await type(message, "And add a test");
    await act(async () => release());
    await settle();
    expect((message as HTMLTextAreaElement).value).toBe("And add a test");
  });

  test("stops a turn that waits for a permission answer", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(chatPath, { transport });
    await screen.findByRole("article", {
      name: "Permission request for go test ./...",
    });
    await click(await screen.findByRole("button", { name: "Stop" }));
    expect(calls.some((c) => c.method === "CancelTurn")).toBe(true);
    expect(await screen.findByText("Turn stopped")).toBeTruthy();
  });

  test("switches between auto-approve and ask in the mode menu, and Esc closes it", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(chatPath, { transport });
    await click(await screen.findByRole("button", { name: "Mode and model" }));
    const dialog = await screen.findByRole("dialog", {
      name: "Mode and model",
    });
    await choose(
      within(dialog).getByRole("combobox", { name: "Permissions" }),
      "Auto-approve",
    );
    expect(
      calls.find((c) => c.method === "SetSessionMode")?.input,
    ).toMatchObject({
      mode: { case: "permissionMode", value: 1 },
    });
    await choose(
      within(dialog).getByRole("combobox", { name: "Model" }),
      "Opus",
    );
    expect(
      calls.find((c) => c.method === "SetSessionConfigOption")?.input,
    ).toMatchObject({
      configOptionId: "model",
      valueId: "opus",
    });
    await act(async () => fireEvent.keyDown(dialog, { key: "Escape" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  test("offers Resume again after a resume waited too long", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/sessions/${MOCK_IDS.pausedSession}`, {
      transport,
    });
    const banner = await screen.findByRole("status", { name: "Session state" });
    expect(within(banner).getByText(/Deleted on/)).toBeTruthy();
    await click(within(banner).getByRole("button", { name: "Resume again" }));
    expect(calls.some((c) => c.method === "ResumeSession")).toBe(true);
    await waitFor(() =>
      expect(
        screen.queryByRole("status", { name: "Session state" }),
      ).toBeNull(),
    );
  });

  test("shows the device-offline banner", async () => {
    renderConsole(`/orgs/${ACME}/sessions/${MOCK_IDS.disconnectedSession}`);
    expect(
      await screen.findByText(/The device of the session is offline/),
    ).toBeTruthy();
  });

  test("replaces the composer of an ended session with a restart from the transcript", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/sessions/${MOCK_IDS.endedSession}`, {
      transport,
    });
    const ended = await screen.findByRole("region", { name: "Session ended" });
    expect(within(ended).getByText("The agent exited.")).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Message" })).toBeNull();
    expect(screen.getByText("The notes are in NOTES.md.")).toBeTruthy();
    await type(
      within(ended).getByRole("textbox", {
        name: "First message of the new session",
      }),
      "Continue",
    );
    await click(
      within(ended).getByRole("button", {
        name: "Start new session from transcript",
      }),
    );
    expect(
      calls.find((c) => c.method === "RestartSessionFromTranscript")?.input,
    ).toMatchObject({
      sessionId: MOCK_IDS.endedSession,
      firstMessage: "Continue",
    });
    await waitFor(() =>
      expect(window.location.pathname).not.toContain(MOCK_IDS.endedSession),
    );
  });

  test("explains an agent installation failure", async () => {
    renderConsole(`/orgs/${ACME}/sessions/${MOCK_IDS.installFailedSession}`);
    expect(
      await screen.findByText(
        /The session could not install the agent in the sandbox/,
      ),
    ).toBeTruthy();
  });

  test("shows only metadata of another member's session", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/sessions/${MOCK_IDS.otherSession}`, {
      transport,
    });
    expect(
      await screen.findByText(
        "This session belongs to another member. Only its owner reads the chat.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Transcript" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete session" })).toBeNull();
    expect(calls.some((c) => c.method === "ListSessionEvents")).toBe(false);
  });

  test("deletes a session after a confirmation", async () => {
    renderConsole(chatPath);
    await click(await screen.findByRole("button", { name: "Delete session" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/transcript permanently/)).toBeTruthy();
    await click(within(dialog).getByRole("button", { name: "Delete session" }));
    await waitFor(() =>
      expect(window.location.pathname).toBe(`/orgs/${ACME}/sessions`),
    );
    const list = await screen.findByRole("list", { name: "Sessions" });
    expect(within(list).queryByText("Fix the flaky upload test")).toBeNull();
  });

  test("shows a not-found notice for a deleted session", async () => {
    renderConsole(
      `/orgs/${ACME}/sessions/0194c200-0000-7000-8000-0000000000ff`,
    );
    expect(
      await screen.findByText(
        "It was not found, or you no longer have access to it.",
      ),
    ).toBeTruthy();
  });
});

describe("settings", () => {
  test("saves a vendor credential without showing or storing its value", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/settings`, { transport });
    await click(
      await screen.findByRole("button", {
        name: "Set the Claude Code credential",
      }),
    );
    const dialog = await screen.findByRole("dialog");
    await choose(
      within(dialog).getByRole("combobox", { name: "Kind" }),
      "Claude Code token",
    );
    const input = within(dialog).getByLabelText(
      "Claude Code token",
    ) as HTMLInputElement;
    expect(input.type).toBe("password");
    await type(input, "sk-ant-oat-secret-value");
    await click(
      within(dialog).getByRole("button", { name: "Save credential" }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const list = screen.getByRole("list", { name: "Your vendor credentials" });
    await waitFor(() =>
      expect(within(list).getByText(/Claude Code token · set/)).toBeTruthy(),
    );
    const set = calls.find((c) => c.method === "SetVendorCredential")!
      .input as {
      value: Uint8Array;
    };
    expect(new TextDecoder().decode(set.value)).toBe("sk-ant-oat-secret-value");
    expect(document.body.textContent).not.toContain("sk-ant-oat-secret-value");
    expect(storedText()).not.toContain("sk-ant-oat-secret-value");
  });

  test("keeps a new credential draft when an earlier dismissed save finishes", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    let release = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const transport: Transport = {
      ...base,
      async unary(method, ...rest) {
        if (method.name === "SetVendorCredential") await held;
        return base.unary(method, ...rest);
      },
    };
    renderConsole(`/orgs/${ACME}/settings`, { transport });
    const open = async () => {
      await click(
        await screen.findByRole("button", {
          name: "Set the Claude Code credential",
        }),
      );
      const dialog = await screen.findByRole("dialog");
      await choose(
        within(dialog).getByRole("combobox", { name: "Kind" }),
        "Claude Code token",
      );
      return dialog;
    };
    let dialog = await open();
    await type(within(dialog).getByLabelText("Claude Code token"), "first");
    await click(
      within(dialog).getByRole("button", { name: "Save credential" }),
    );
    await act(async () => fireEvent.keyDown(dialog, { key: "Escape" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    dialog = await open();
    const input = within(dialog).getByLabelText(
      "Claude Code token",
    ) as HTMLInputElement;
    await type(input, "second");
    await act(async () => release());
    await settle();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(input.value).toBe("second");
  });

  test("rejects an auth.json file that is not a JSON object", async () => {
    renderConsole(`/orgs/${ACME}/settings`);
    await click(
      await screen.findByRole("button", {
        name: "Set the OpenAI Codex credential",
      }),
    );
    const dialog = await screen.findByRole("dialog");
    await choose(
      within(dialog).getByRole("combobox", { name: "Kind" }),
      "Codex auth.json",
    );
    const input = within(dialog).getByLabelText(
      "auth.json file",
    ) as HTMLInputElement;
    const file = new File(["[1]"], "auth.json", { type: "application/json" });
    await act(async () => {
      fireEvent.change(input, { target: { files: [file] } });
    });
    await settle();
    await click(
      within(dialog).getByRole("button", { name: "Save credential" }),
    );
    expect(
      await within(dialog).findByText("The file is not a JSON object."),
    ).toBeTruthy();
  });

  test("drops a credential file read that ends after the dialog closes", async () => {
    renderConsole(`/orgs/${ACME}/settings`);
    const open = async () => {
      await click(
        await screen.findByRole("button", {
          name: "Set the OpenAI Codex credential",
        }),
      );
      const dialog = await screen.findByRole("dialog");
      await choose(
        within(dialog).getByRole("combobox", { name: "Kind" }),
        "Codex auth.json",
      );
      return dialog;
    };
    let dialog = await open();
    const file = new File(['{"OPENAI_API_KEY":"k"}'], "auth.json", {
      type: "application/json",
    });
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const read = file.arrayBuffer.bind(file);
    file.arrayBuffer = async () => {
      await held;
      return read();
    };
    await act(async () => {
      fireEvent.change(within(dialog).getByLabelText("auth.json file"), {
        target: { files: [file] },
      });
    });
    await act(async () => fireEvent.keyDown(dialog, { key: "Escape" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    dialog = await open();
    await act(async () => release());
    await settle();
    expect(within(dialog).queryByText("Selected: auth.json")).toBeNull();
  });

  test("does not save an earlier file while its replacement loads", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/settings`, { transport });
    await click(
      await screen.findByRole("button", {
        name: "Set the OpenAI Codex credential",
      }),
    );
    const dialog = await screen.findByRole("dialog");
    await choose(
      within(dialog).getByRole("combobox", { name: "Kind" }),
      "Codex auth.json",
    );
    const input = within(dialog).getByLabelText("auth.json file");
    const first = new File(['{"account":"a"}'], "a.json", {
      type: "application/json",
    });
    await act(async () => {
      fireEvent.change(input, { target: { files: [first] } });
    });
    await settle();
    expect(within(dialog).getByText("Selected: a.json")).toBeTruthy();
    const second = new File(['{"account":"b"}'], "b.json", {
      type: "application/json",
    });
    second.arrayBuffer = () => new Promise(() => {});
    await act(async () => {
      fireEvent.change(input, { target: { files: [second] } });
    });
    await click(
      within(dialog).getByRole("button", { name: "Save credential" }),
    );
    expect(within(dialog).queryByText("Selected: a.json")).toBeNull();
    expect(calls.some((c) => c.method === "SetVendorCredential")).toBe(false);
  });

  test("adds a session secret and shows only its name", async () => {
    renderConsole(`/orgs/${ACME}/settings`);
    await click(await screen.findByRole("button", { name: "Add secret" }));
    const dialog = await screen.findByRole("dialog");
    const name = within(dialog).getByRole("textbox", { name: "Name" });
    await waitFor(() => expect(document.activeElement).toBe(name));
    await type(name, "SANDCREW_X");
    await type(within(dialog).getByLabelText("Value"), "super-secret");
    await click(within(dialog).getByRole("button", { name: "Save secret" }));
    expect(
      await within(dialog).findByText(
        "Names that start with SANDCREW_ are reserved.",
      ),
    ).toBeTruthy();
    await type(name, "NPM_TOKEN");
    await click(within(dialog).getByRole("button", { name: "Save secret" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const list = screen.getByRole("list", { name: "Session secrets" });
    await waitFor(() =>
      expect(within(list).getByText("NPM_TOKEN")).toBeTruthy(),
    );
    expect(document.body.textContent).not.toContain("super-secret");
    expect(storedText()).not.toContain("super-secret");
  });

  test("tells a browser without Web Push support", async () => {
    renderConsole(`/orgs/${ACME}/settings`);
    expect(
      await screen.findByText("This browser does not support notifications."),
    ).toBeTruthy();
  });

  test("saves the organization settings with a field mask", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/settings/organization`, { transport });
    const windows = await screen.findByRole("checkbox", {
      name: "Windows 4 vCPU",
    });
    expect(
      windows.getAttribute("aria-disabled") ??
        String((windows as HTMLButtonElement).disabled),
    ).toMatch(/true/);
    expect(
      screen.getByText(/SandCrew does not support this guest OS yet/),
    ).toBeTruthy();
    expect(
      screen.getByRole("checkbox", {
        name: `Missing profile ${MOCK_IDS.deletedProfile}`,
      }),
    ).toBeTruthy();
    await click(
      screen.getByRole("checkbox", {
        name: `Missing profile ${MOCK_IDS.deletedProfile}`,
      }),
    );
    await choose(
      screen.getByRole("combobox", { name: "Paused-session retention" }),
      "Unlimited",
    );
    await click(screen.getByRole("button", { name: "Save settings" }));
    const update = calls.find((c) => c.method === "UpdateOrganizationSettings")!
      .input as {
      updateMask: { paths: string[] };
      settings: { allowedRunnerProfileIds: string[] };
    };
    expect(update.updateMask.paths).toEqual([
      "allowed_runner_profile_ids",
      "paused_session_retention",
    ]);
    expect(update.settings.allowedRunnerProfileIds).toEqual([
      MOCK_IDS.linuxProfile,
      MOCK_IDS.macosProfile,
    ]);
    expect(await screen.findByText("Settings saved")).toBeTruthy();
  });

  test("compares edits with the latest save, even within one second", async () => {
    const { transport, calls } = recording(
      createMockTransport({ agentStepMs: 5 }),
    );
    renderConsole(`/orgs/${ACME}/settings/organization`, { transport });
    const saves = () =>
      calls.filter((c) => c.method === "UpdateOrganizationSettings");
    const setDelay = async (minutes: string) => {
      const delay = await screen.findByRole("textbox", {
        name: "Pause delay in minutes",
      });
      await type(delay, minutes);
      await act(async () => fireEvent.blur(delay));
    };
    const save = async (count: number) => {
      await click(screen.getByRole("button", { name: "Save settings" }));
      await waitFor(() => expect(saves()).toHaveLength(count));
      await waitFor(() =>
        expect(
          (
            screen.getByRole("button", {
              name: "Save settings",
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(true),
      );
    };
    await setDelay("20");
    await save(1);
    await setDelay("40");
    await save(2);
    // The server holds 40 minutes, so 20 minutes is a change again.
    await setDelay("20");
    await save(3);
    expect(saves()[2]!.input).toMatchObject({
      settings: { pauseDelay: { seconds: 1200n } },
    });
  });

  test("locks the settings while a save is pending", async () => {
    const base = createMockTransport({ agentStepMs: 5 });
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const transport: Transport = {
      ...base,
      async unary(method, ...rest) {
        if (method.name === "UpdateOrganizationSettings") await held;
        return base.unary(method, ...rest);
      },
    };
    renderConsole(`/orgs/${ACME}/settings/organization`, { transport });
    const delay = await screen.findByRole("textbox", {
      name: "Pause delay in minutes",
    });
    await type(delay, "20");
    await act(async () => fireEvent.blur(delay));
    await click(screen.getByRole("button", { name: "Save settings" }));
    const controls = () => [
      screen.getByRole("textbox", { name: "Pause delay in minutes" }),
      screen.getByRole("combobox", { name: "Paused-session retention" }),
      ...screen.getAllByRole("checkbox"),
    ];
    for (const control of controls()) {
      expect(
        control.matches(":disabled, [aria-disabled='true'], [data-disabled]"),
      ).toBe(true);
    }
    await act(async () => release());
    await waitFor(() =>
      expect(
        (
          screen.getByRole("textbox", {
            name: "Pause delay in minutes",
          }) as HTMLInputElement
        ).disabled,
      ).toBe(false),
    );
  });

  test("requires a pause delay", async () => {
    renderConsole(`/orgs/${ACME}/settings/organization`);
    const delay = await screen.findByRole("textbox", {
      name: "Pause delay in minutes",
    });
    await type(delay, "");
    await act(async () => fireEvent.blur(delay));
    await click(screen.getByRole("button", { name: "Save settings" }));
    expect(
      await screen.findByText(
        "Enter a whole number of minutes from 1 to 1440.",
      ),
    ).toBeTruthy();
  });
});

describe("web push opt-in", () => {
  function renderSettings(env: PushEnvironment) {
    const transport = createMockTransport({ agentStepMs: 5 });
    const organization = create(OrganizationSchema, {
      organizationId: ACME,
      name: "Acme Robotics",
      callerRole: Role.OWNER,
    });
    render(
      <AuthContextProvider value={auth()}>
        <TransportProvider transport={transport}>
          <QueryClientProvider client={testQueryClient()}>
            <RouterProvider>
              <OrgContextProvider
                value={{
                  organizationId: ACME,
                  organization,
                  role: Role.OWNER,
                  userId: MOCK_USER,
                  leaveOnNotFound: () => false,
                }}
              >
                <UserSettingsPage pushEnvironment={env} />
              </OrgContextProvider>
            </RouterProvider>
          </QueryClientProvider>
        </TransportProvider>
      </AuthContextProvider>,
    );
  }

  test("asks for permission only after the click and lists the subscription", async () => {
    let asked = 0;
    const env: PushEnvironment = {
      hasPushManager: true,
      notification: {
        permission: "default",
        requestPermission: async () => {
          asked++;
          return "granted";
        },
      },
      serviceWorker: {
        register: async () =>
          ({
            pushManager: {
              subscribe: async () => ({
                endpoint: "https://push.example.com/send/abc",
                getKey: () => new Uint8Array([1, 2, 3]).buffer,
              }),
            },
          }) as unknown as ServiceWorkerRegistration,
      },
    };
    renderSettings(env);
    const button = await screen.findByRole("button", {
      name: "Turn on notifications on this browser",
    });
    expect(asked).toBe(0);
    await click(button);
    expect(asked).toBe(1);
    const list = await screen.findByRole("list", {
      name: "Notification subscriptions",
    });
    expect(within(list).getByText(/https:\/\/push.example.com/)).toBeTruthy();
    await click(
      within(list).getByRole("button", { name: /Turn off notifications/ }),
    );
    const dialog = await screen.findByRole("alertdialog");
    await click(within(dialog).getByRole("button", { name: "Turn off" }));
    expect(
      await screen.findByText("No browser gets notifications"),
    ).toBeTruthy();
  });

  test("explains a permission that the user did not grant", async () => {
    renderSettings({
      hasPushManager: true,
      notification: {
        permission: "default",
        requestPermission: async () => "denied",
      },
      serviceWorker: {
        register: async () => {
          throw new Error("not called");
        },
      },
    });
    await click(
      await screen.findByRole("button", {
        name: "Turn on notifications on this browser",
      }),
    );
    expect(
      await screen.findByText(/Notifications are not allowed for this site/),
    ).toBeTruthy();
  });

  test("tells a blocked browser how to allow notifications", async () => {
    renderSettings({
      hasPushManager: true,
      notification: {
        permission: "denied",
        requestPermission: async () => "denied",
      },
      serviceWorker: {
        register: async () => {
          throw new ConnectError("x");
        },
      },
    });
    expect(
      await screen.findByText(/Notifications are blocked for this site/),
    ).toBeTruthy();
    expect(
      screen.queryByRole("button", {
        name: "Turn on notifications on this browser",
      }),
    ).toBeNull();
  });
});

describe("transcript scrolling", () => {
  const chunks = (...values: string[]) =>
    buildTranscript(
      values.map((value, index) =>
        create(TranscriptEventSchema, {
          sequence: BigInt(index + 1),
          event: {
            case: "agentMessageChunk",
            value: { content: { block: { case: "text", value } } },
          },
        }),
      ),
    );

  test("follows a streamed message that grows, until the user scrolls away", () => {
    const scrolls: unknown[] = [];
    const original = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = function (arg) {
      scrolls.push(arg);
    };
    try {
      const view = (values: string[]) => (
        <TranscriptView
          transcript={chunks(...values)}
          loaded
          loadError={undefined}
          onRetry={() => {}}
        />
      );
      const { rerender, container } = render(view(["Hi"]));
      expect(scrolls).toHaveLength(1);
      // The chunk merges into the same message: no new item, but new output.
      rerender(view(["Hi", " there"]));
      expect(scrolls).toHaveLength(2);
      // The user scrolls up: the end of the transcript is far below the window.
      const marker = container.querySelector(".chat")!.lastElementChild!;
      marker.getBoundingClientRect = () =>
        ({ top: window.innerHeight + 2000 }) as DOMRect;
      fireEvent.scroll(window);
      rerender(view(["Hi", " there", "!"]));
      expect(scrolls).toHaveLength(2);
    } finally {
      Element.prototype.scrollIntoView = original;
    }
  });
});
