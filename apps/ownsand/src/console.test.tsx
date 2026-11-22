// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { StrictMode } from "react";
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
import {
  type DeviceLimitsInput,
  type Resources,
} from "@delino/ownsand-protos/v1/common_pb";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { QueryClient } from "@tanstack/react-query";
import { Console } from "./App.tsx";
import { createQueryClient } from "./api.ts";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { AuthMode, type ConsoleConfig } from "./config.ts";
import { pendingInvitation } from "./invitation.ts";
import { RouteId, RouterProvider, useRouter } from "./router.tsx";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
  MOCK_USER,
  MockBillingState,
} from "./mock/mock-api.ts";

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
    strict?: boolean;
  } = {},
) {
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

  test("requests the first 200 organizations on the page and in the switcher", async () => {
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
    renderConsole("/", { transport });
    expect(await screen.findByRole("link", { name: /Extra 49/ })).toBeTruthy();
    expect(pageSizes.length).toBeGreaterThan(0);
    expect(pageSizes.every((size) => size === 200)).toBe(true);
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
      fireEvent.click(within(row).getByRole("button", { name: "Revoke" }));
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

  /** Delays AcceptInvitation until the returned function releases it. */
  function delayedAcceptTransport() {
    const mock = createMockTransport();
    let release = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "AcceptInvitation") {
          await gate;
        }
        return mock.unary(method, ...rest);
      },
    };
    return { transport, release };
  }

  test("does not navigate when an earlier accept succeeds after a new link opened", async () => {
    const { transport, release } = delayedAcceptTransport();
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await act(async () => {
      window.location.hash = `secret=${MOCK_ACME_INVITATION_SECRET}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    release();
    await settle(60);
    expect(window.location.pathname).toBe("/invitations/accept");
    expect(
      screen.getByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
  });

  /** Counts ListOrganizations calls. */
  function listOrganizationsProbe() {
    const mock = createMockTransport();
    const state = { calls: 0 };
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "ListOrganizations") {
          state.calls += 1;
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
    const { transport, release } = delayedAcceptTransport();
    pendingInvitation.save(MOCK_INVITATION_SECRET);
    renderConsole("/invitations/accept", { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("link", { name: "Not now" }));
    });
    expect(window.location.pathname).not.toBe("/invitations/accept");
    const leftPath = window.location.pathname;
    release();
    await settle(60);
    expect(window.location.pathname).toBe(leftPath);
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

  test("shows on every organization page", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
      transport: createMockTransport({
        billing: { [MOCK_IDS.acme]: MockBillingState.BlockedLinked },
      }),
    });
    expect(
      await screen.findByRole("region", {
        name: "This organization is blocked",
      }),
    ).toBeTruthy();
  });
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
