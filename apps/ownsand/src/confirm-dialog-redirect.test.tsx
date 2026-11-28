// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { Role } from "@delino/ownsand-protos/v1/common_pb";
import {
  OrganizationService,
  type GetOrganizationResponse,
} from "@delino/ownsand-protos/v1/organization_pb";
import {
  ListSandboxesResponseSchema,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import {
  ListSnapshotsResponseSchema,
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
import { createMockTransport, MOCK_IDS } from "./mock/mock-api.ts";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

/** One flow whose confirmed request redirects when its opening is current. */
interface Flow {
  name: string;
  /** The RPC that the confirmation sends. */
  method: string;
  /** The page that the flow starts on. */
  path: string;
  userId?: string;
  /** The button that opens the confirmation, and the dialog's confirm button. */
  opener: string;
  confirm: string;
  /** Where a current opening redirects to. */
  redirect: string;
  /** A query that the success refreshes while the user is elsewhere. */
  refreshed: string;
  /**
   * Whether the page survives the success. Leaving or deleting the
   * organization makes the refreshed organization queries fail with
   * not_found, which replaces the page.
   */
  pageSurvives: boolean;
}

const FLOWS: Flow[] = [
  {
    name: "device removal",
    method: "RemoveDevice",
    path: `/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`,
    opener: "Remove device",
    confirm: "Remove device",
    redirect: `/orgs/${MOCK_IDS.acme}/devices`,
    refreshed: "ListDevices",
    pageSurvives: true,
  },
  {
    name: "organization deletion",
    method: "DeleteOrganization",
    path: `/orgs/${MOCK_IDS.acme}/settings`,
    opener: "Delete organization",
    confirm: "Delete organization",
    redirect: "/",
    refreshed: "ListOrganizations",
    pageSurvives: false,
  },
  {
    name: "self-removal",
    method: "RemoveMember",
    path: `/orgs/${MOCK_IDS.northwind}/members`,
    opener: "Leave organization",
    confirm: "Leave organization",
    redirect: "/",
    refreshed: "ListOrganizations",
    pageSurvives: false,
  },
];

test("device removal redirects while the detail refresh is pending", async () => {
  const devicesPath = `/orgs/${MOCK_IDS.acme}/devices`;
  const userId = "user-admin-jun";
  let holdDetail = false;
  const { transport, calls, seen } = holdingTransport(
    ["GetDevice"],
    () => holdDetail,
    createMockTransport({ userId }),
  );
  renderConsole(`${devicesPath}/${MOCK_IDS.buildBox}`, { transport, userId });

  fireEvent.click(await screen.findByRole("button", { name: "Remove device" }));
  const dialog = await screen.findByRole("alertdialog");
  const entries = window.history.length;
  try {
    holdDetail = true;
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Remove device" }),
    );

    await waitFor(() => expect(calls.GetDevice).toHaveLength(1));
    expect(
      await screen.findByRole("link", { name: "studio-mac" }),
    ).toBeTruthy();
    expect(window.location.pathname).toBe(devicesPath);
    expect(window.history.length).toBe(entries + 1);
    expect(screen.queryByRole("link", { name: "build-box-1" })).toBeNull();
    expect(seen.RemoveDevice).toBe(1);
  } finally {
    await act(async () => {
      for (const call of calls.GetDevice ?? []) call.settle();
    });
  }
});

describe("device removal permission refresh", () => {
  const organizationId = MOCK_IDS.acme;
  const devicesPath = `/orgs/${organizationId}/devices`;
  const detailPath = `${devicesPath}/${MOCK_IDS.buildBox}`;
  const removeButton = { name: "Remove device" };

  async function start() {
    const userId = "user-admin-jun";
    const mock = createMockTransport({ userId });
    const queryClient = new QueryClient();
    let callerRole = Role.ADMIN;
    const roleTransport: Transport = {
      ...mock,
      async unary(method, ...args) {
        const response = await mock.unary(method, ...args);
        if (method.name !== "GetOrganization") return response;
        const { organization } = response.message as GetOrganizationResponse;
        if (organization?.organizationId !== organizationId) return response;
        return {
          ...response,
          message: {
            ...response.message,
            organization: { ...organization, callerRole },
          },
        };
      },
    };
    const held = holdingTransport(["RemoveDevice"], undefined, roleTransport);
    async function refreshRole(role: Role) {
      callerRole = role;
      await act(async () => {
        await queryClient.refetchQueries({
          queryKey: createConnectQueryKey({
            schema: OrganizationService.method.getOrganization,
            input: { organizationId },
            cardinality: "finite",
          }),
        });
      });
    }
    renderConsole(detailPath, {
      transport: held.transport,
      queryClient,
      userId,
    });
    fireEvent.click(await screen.findByRole("button", removeButton));
    const dialog = await screen.findByRole("alertdialog");
    return { ...held, queryClient, refreshRole, dialog };
  }

  test("a refresh that retains removal permission keeps the same confirmation", async () => {
    const { calls, dialog, refreshRole } = await start();
    await refreshRole(Role.OWNER);
    await screen.findByText("· Owner");
    expect(screen.getByRole("alertdialog")).toBe(dialog);
    fireEvent.click(within(dialog).getByRole("button", removeButton));
    await waitFor(() => expect(calls.RemoveDevice).toHaveLength(1));
    await act(() => calls.RemoveDevice![0]!.settle());
    await waitFor(() => expect(window.location.pathname).toBe(devicesPath));
    expect(calls.RemoveDevice).toHaveLength(1);
  });

  test("demotion closes the confirmation until the opener is used again", async () => {
    const { dialog, refreshRole, seen } = await start();
    const confirm = within(dialog).getByRole("button", removeButton);
    await refreshRole(Role.MEMBER);
    await screen.findByText("· Member");
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(screen.queryByRole("button", removeButton)).toBeNull();
    const heading = screen.getByRole("heading", { name: "build-box-1" });
    fireEvent.keyDown(heading, { key: "Enter" });
    fireEvent.click(confirm);
    expect(seen.RemoveDevice ?? 0).toBe(0);

    await refreshRole(Role.ADMIN);
    const opener = await screen.findByRole("button", removeButton);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    fireEvent.click(opener);
    await screen.findByRole("alertdialog");
    expect(seen.RemoveDevice ?? 0).toBe(0);
  });

  test("a late success refreshes all affected caches and leaves the route in place", async () => {
    const { calls, dialog, queryClient, refreshRole } = await start();
    const sandboxes = createConnectQueryKey({
      schema: SandboxService.method.listSandboxes,
      input: { organizationId },
      cardinality: "finite",
    });
    const snapshots = createConnectQueryKey({
      schema: SnapshotService.method.listSnapshots,
      input: { organizationId },
      cardinality: "finite",
    });
    queryClient.setQueryData(sandboxes, create(ListSandboxesResponseSchema));
    queryClient.setQueryData(snapshots, create(ListSnapshotsResponseSchema));
    const entries = window.history.length;
    fireEvent.click(within(dialog).getByRole("button", removeButton));
    await waitFor(() => expect(calls.RemoveDevice).toHaveLength(1));
    await refreshRole(Role.MEMBER);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await act(() => calls.RemoveDevice![0]!.settle());
    await screen.findByText("Removed", { exact: true });
    expect(window.location.pathname).toBe(detailPath);
    expect(window.history.length).toBe(entries);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(queryClient.getQueryState(sandboxes)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(snapshots)?.isInvalidated).toBe(true);
  });

  test("a late failure leaves a newer confirmation free of errors after recovery", async () => {
    const { calls, dialog, queryClient, refreshRole } = await start();
    fireEvent.click(within(dialog).getByRole("button", removeButton));
    await waitFor(() => expect(calls.RemoveDevice).toHaveLength(1));
    await refreshRole(Role.MEMBER);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await refreshRole(Role.ADMIN);
    fireEvent.click(await screen.findByRole("button", removeButton));
    const current = await screen.findByRole("alertdialog");
    await act(() =>
      calls.RemoveDevice![0]!.settle(
        new ConnectError("the earlier removal failed", Code.PermissionDenied),
      ),
    );
    await waitFor(() =>
      expect(queryClient.getMutationCache().getAll()[0]?.state.status).toBe(
        "error",
      ),
    );
    expect(window.location.pathname).toBe(detailPath);
    expect(screen.getByRole("alertdialog")).toBe(current);
    expect(current.querySelector(".notice-error")).toBeNull();
  });
});

describe.each(FLOWS)("$name confirmation redirect", (flow) => {
  async function start(shouldHoldDeviceRefresh = () => false) {
    const held = holdingTransport(
      [flow.method, "GetDevice"],
      (method) => method === flow.method || shouldHoldDeviceRefresh(),
    );
    renderConsole(flow.path, {
      transport: held.transport,
      userId: flow.userId,
    });
    return held;
  }

  async function open() {
    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: flow.opener }));
    });
    await settle();
    return screen.findByRole("alertdialog");
  }

  async function confirm(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", { name: flow.confirm }),
      );
    });
    await settle();
  }

  async function escape(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
  }

  async function release(
    held: Awaited<ReturnType<typeof start>>,
    error?: Error,
  ) {
    await act(async () => {
      held.calls[flow.method]?.[0]?.settle(error);
    });
    await settle(100);
  }

  /** Goes to the Sandboxes page of the organization, as a user who left the dialog. */
  async function goElsewhere() {
    await act(async () => {
      fireEvent.click(await screen.findByRole("link", { name: "Sandboxes" }));
    });
    await settle(60);
    return window.location.pathname;
  }

  test("a request that succeeds after Escape does not move the user", async () => {
    const held = await start();
    const dialog = await open();
    await confirm(dialog);
    expect(held.calls[flow.method]).toHaveLength(1);
    await escape(dialog);
    const elsewhere = await goElsewhere();
    const entries = window.history.length;
    const before = held.seen[flow.refreshed] ?? 0;
    await release(held);
    expect(window.location.pathname).toBe(elsewhere);
    expect(window.history.length).toBe(entries);
    // The cache work of the success still runs for the dismissed opening.
    expect(held.seen[flow.refreshed] ?? 0).toBeGreaterThan(before);
  });

  test("a request that succeeds after Cancel does not move the user", async () => {
    const held = await start();
    const dialog = await open();
    await confirm(dialog);
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    });
    await settle(60);
    const elsewhere = await goElsewhere();
    const entries = window.history.length;
    await release(held);
    expect(window.location.pathname).toBe(elsewhere);
    expect(window.history.length).toBe(entries);
  });

  test("a request that succeeds while a newer confirmation is open does not move the user or close it", async () => {
    let holdDeviceRefresh = false;
    const held = await start(() => holdDeviceRefresh);
    const first = await open();
    await confirm(first);
    await escape(first);
    const path = window.location.pathname;
    const entries = window.history.length;
    const second = await open();
    // Hold the device readback so the device stays eligible during this
    // assertion. A readback that shows it was removed closes the dialog.
    holdDeviceRefresh = true;
    try {
      await release(held);
      expect(window.location.pathname).toBe(path);
      expect(window.history.length).toBe(entries);
      if (flow.pageSurvives) {
        await waitFor(() => expect(held.calls.GetDevice).toHaveLength(1));
        expect(screen.getByRole("alertdialog")).toBe(second);
        expect(
          (
            within(second).getByRole("button", {
              name: flow.confirm,
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(false);
      }
    } finally {
      await act(async () => {
        for (const call of held.calls.GetDevice ?? []) call.settle();
      });
    }
  });

  test("a request that succeeds while its confirmation is open redirects", async () => {
    const held = await start();
    const dialog = await open();
    await confirm(dialog);
    const entries = window.history.length;
    await release(held);
    expect(window.location.pathname).toBe(flow.redirect);
    expect(window.history.length).toBe(entries + 1);
  });

  test("a failed request keeps the dialog with its notice and a retry can succeed", async () => {
    const held = await start();
    const dialog = await open();
    await confirm(dialog);
    await release(
      held,
      new ConnectError("the request failed", Code.Unavailable),
    );
    expect(window.location.pathname).toBe(
      new URL(flow.path, "http://x").pathname,
    );
    expect(dialog.querySelector(".notice-error")).not.toBeNull();
    await confirm(dialog);
    expect(held.calls[flow.method]).toHaveLength(2);
    await act(async () => {
      held.calls[flow.method]?.[1]?.settle();
    });
    await settle(100);
    expect(window.location.pathname).toBe(flow.redirect);
  });
});
