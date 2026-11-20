// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { Console } from "./App.tsx";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { AuthMode } from "./config.ts";
import { pendingInvitation } from "./invitation.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
  MOCK_USER,
} from "./mock/mock-api.ts";

const config = {
  apiOrigin: "https://api.test",
  authMode: AuthMode.Mock,
  logto: undefined,
};

function renderConsole(path: string, options: { signedIn?: boolean } = {}) {
  window.history.replaceState(null, "", path);
  const signIns: number[] = [];
  const signedIn = options.signedIn ?? true;
  const auth: Auth = {
    isLoading: false,
    isAuthenticated: signedIn,
    userId: signedIn ? MOCK_USER : undefined,
    signIn: async () => {
      signIns.push(1);
    },
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
        transport={createMockTransport()}
        queryClient={queryClient}
      />
    </AuthContextProvider>,
  );
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

  test("deletes the stored secret also when AcceptInvitation fails", async () => {
    pendingInvitation.save("osi_wrong");
    renderConsole("/invitations/accept");
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await settle(60);
    expect(pendingInvitation.read()).toBeUndefined();
  });
});
