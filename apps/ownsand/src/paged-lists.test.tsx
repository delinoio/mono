// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

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

describe("enrollment tokens list", () => {
  /** A mock whose acme organization holds the seeded token and `count` created ones. */
  async function withTokens(count: number) {
    const mock = createMockTransport();
    const client = createClient(DeviceService, mock);
    for (let i = 0; i < count; i++) {
      await client.createEnrollmentToken({
        organizationId: MOCK_IDS.acme,
        ttl: { seconds: 3600n },
      });
    }
    return mock;
  }

  test("shows the seeded token from one request of 200 rows", async () => {
    const { transport, state } = pagedTransport(
      "ListEnrollmentTokens",
      createMockTransport(),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    expect(await screen.findByText(/ose_91c2/)).toBeTruthy();
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
    expect(await screen.findByText("No enrollment tokens")).toBeTruthy();
    expect(
      screen.queryByRole("list", { name: "Enrollment tokens" }),
    ).toBeNull();
  });

  test("lists every page and revokes a new token that sorts after the first page", async () => {
    // 1 seeded and 204 created tokens: the second page holds 5 tokens.
    const { transport, state } = pagedTransport(
      "ListEnrollmentTokens",
      await withTokens(204),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    await screen.findByRole("list", { name: "Enrollment tokens" });
    await settle(100);
    expect(itemsOf("Enrollment tokens")).toHaveLength(205);
    expect(state.tokens).toHaveLength(2);
    expect(state.tokens[0]).toBe("");
    expect(state.tokens[1]).not.toBe("");

    // Create one more token in the console. It is the last row.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create token" }));
    });
    const form = await screen.findByRole("dialog", {
      name: "Create an enrollment token",
    });
    await act(async () => {
      fireEvent.click(
        within(form).getByRole("button", { name: "Create token" }),
      );
    });
    await settle(100);
    const secret = await screen.findByRole("dialog", {
      name: "Your new enrollment token",
    });
    await act(async () => {
      fireEvent.keyDown(secret, { key: "Escape" });
    });
    await settle(100);
    const rows = itemsOf("Enrollment tokens");
    expect(rows).toHaveLength(206);
    const last = rows[rows.length - 1]!;
    expect(last.textContent).toContain("Not used");

    await act(async () => {
      fireEvent.click(within(last).getByRole("button", { name: "Revoke" }));
    });
    await settle(100);
    const after = itemsOf("Enrollment tokens");
    expect(after).toHaveLength(206);
    expect(after[after.length - 1]!.textContent).toContain("Revoked");
    expect(
      within(after[after.length - 1]!).queryByRole("button", {
        name: "Revoke",
      }),
    ).toBeNull();
  });

  test("keeps the loaded tokens and offers Try again when a later page fails", async () => {
    const { transport, state } = pagedTransport(
      "ListEnrollmentTokens",
      await withTokens(204),
    );
    state.failLaterPages = true;
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    expect(
      await screen.findByText("Some enrollment tokens are not shown yet."),
    ).toBeTruthy();
    expect(itemsOf("Enrollment tokens")).toHaveLength(200);
    state.failLaterPages = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    await settle(100);
    expect(itemsOf("Enrollment tokens")).toHaveLength(205);
    expect(
      screen.queryByText("Some enrollment tokens are not shown yet."),
    ).toBeNull();
  });

  test("shows no token of the previous organization after the organization changes", async () => {
    const mock = await withTokens(204);
    const { organization } = await createClient(
      OrganizationService,
      mock,
    ).createOrganization({ name: "Initech" });
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, {
      transport: mock,
    });
    await screen.findByRole("list", { name: "Enrollment tokens" });
    await settle(100);
    expect(itemsOf("Enrollment tokens")).toHaveLength(205);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    await act(async () => {
      fireEvent.click(await screen.findByRole("option", { name: "Initech" }));
    });
    await settle(100);
    expect(window.location.pathname).toBe(
      `/orgs/${organization!.organizationId}/enrollment-tokens`,
    );
    expect(await screen.findByText("No enrollment tokens")).toBeTruthy();
    expect(
      screen.queryByRole("list", { name: "Enrollment tokens" }),
    ).toBeNull();
  });
});

describe("API keys list", () => {
  /** A mock whose acme organization holds the seeded key and `count` created ones. */
  async function withKeys(count: number) {
    const mock = createMockTransport();
    const client = createClient(ApiKeyService, mock);
    for (let i = 0; i < count; i++) {
      await client.createApiKey({
        organizationId: MOCK_IDS.acme,
        name: `key-${i}`,
      });
    }
    return mock;
  }

  test("shows the seeded key from one request of 200 rows", async () => {
    const { transport, state } = pagedTransport(
      "ListApiKeys",
      createMockTransport(),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    expect(await screen.findByText(/ci-runner/)).toBeTruthy();
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
    expect(await screen.findByText("No API keys")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "API keys" })).toBeNull();
  });

  test("lists every page and revokes a new key that sorts after the first page", async () => {
    // 1 seeded and 204 created keys: the second page holds 5 keys.
    const { transport, state } = pagedTransport(
      "ListApiKeys",
      await withKeys(204),
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    await screen.findByRole("list", { name: "API keys" });
    await settle(100);
    expect(itemsOf("API keys")).toHaveLength(205);
    expect(state.tokens).toHaveLength(2);
    expect(state.tokens[0]).toBe("");
    expect(state.tokens[1]).not.toBe("");

    // Create one more key in the console. It is the last row.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create API key" }));
    });
    const form = await screen.findByRole("dialog", {
      name: "Create an API key",
    });
    await act(async () => {
      fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), {
        target: { value: "late-key" },
      });
    });
    await act(async () => {
      fireEvent.click(within(form).getByRole("button", { name: "Create key" }));
    });
    await settle(100);
    const secret = await screen.findByRole("dialog", {
      name: "Your new API key",
    });
    await act(async () => {
      fireEvent.keyDown(secret, { key: "Escape" });
    });
    await settle(100);
    const rows = itemsOf("API keys");
    expect(rows).toHaveLength(206);
    const last = rows[rows.length - 1]!;
    expect(last.textContent).toContain("late-key");

    await act(async () => {
      fireEvent.click(within(last).getByRole("button", { name: "Revoke" }));
    });
    await settle(60);
    const dialog = await screen.findByRole("alertdialog", {
      name: "Revoke late-key?",
    });
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Revoke key" }),
      );
    });
    await settle(100);
    const after = itemsOf("API keys");
    expect(after).toHaveLength(206);
    const revoked = after[after.length - 1]!;
    expect(revoked.textContent).toContain("late-key");
    expect(revoked.textContent).toContain("Revoked");
    expect(
      within(revoked).queryByRole("button", { name: "Revoke" }),
    ).toBeNull();
  });

  test("keeps the loaded keys and offers Try again when a later page fails", async () => {
    const { transport, state } = pagedTransport(
      "ListApiKeys",
      await withKeys(204),
    );
    state.failLaterPages = true;
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    expect(
      await screen.findByText("Some API keys are not shown yet."),
    ).toBeTruthy();
    expect(itemsOf("API keys")).toHaveLength(200);
    state.failLaterPages = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    await settle(100);
    expect(itemsOf("API keys")).toHaveLength(205);
    expect(screen.queryByText("Some API keys are not shown yet.")).toBeNull();
  });

  test("shows no key of the previous organization after the organization changes", async () => {
    const mock = await withKeys(204);
    const { organization } = await createClient(
      OrganizationService,
      mock,
    ).createOrganization({ name: "Initech" });
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport: mock });
    await screen.findByRole("list", { name: "API keys" });
    await settle(100);
    expect(itemsOf("API keys")).toHaveLength(205);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    await act(async () => {
      fireEvent.click(await screen.findByRole("option", { name: "Initech" }));
    });
    await settle(100);
    expect(window.location.pathname).toBe(
      `/orgs/${organization!.organizationId}/api-keys`,
    );
    expect(await screen.findByText("No API keys")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "API keys" })).toBeNull();
    expect(screen.queryByText(/key-0/)).toBeNull();
  });
});
