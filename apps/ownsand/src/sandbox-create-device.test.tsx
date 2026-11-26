// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import { DeviceState } from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import type { CreateSandboxRequest } from "@delino/ownsand-protos/v1/sandbox_pb";
import { deviceOptionLabel, errorMessage } from "./format.ts";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("device option label", () => {
  test("deviceOptionLabel appends the state name in parentheses", () => {
    expect(deviceOptionLabel("build-box-1", DeviceState.ONLINE)).toBe(
      "build-box-1 (Online)",
    );
    expect(deviceOptionLabel("studio-mac", DeviceState.OFFLINE)).toBe(
      "studio-mac (Offline)",
    );
    expect(deviceOptionLabel("old", DeviceState.REMOVED)).toBe("old (Removed)");
    expect(deviceOptionLabel("x", DeviceState.UNSPECIFIED)).toBe("x (Unknown)");
    // A state value that this build does not know reads as Unknown.
    expect(deviceOptionLabel("x", 99 as DeviceState)).toBe("x (Unknown)");
  });

  test("errorMessage keeps the generic text for Unavailable", () => {
    expect(errorMessage(new ConnectError("x", Code.Unavailable))).toBe(
      "OwnSand is not available right now. Try again in a moment.",
    );
  });
});

describe("create sandbox device select", () => {
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

  async function chooseDevice(dialog: HTMLElement, name: string) {
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("combobox", { name: "Device" }));
    });
    const option = await screen.findByRole(
      "option",
      { name },
      { timeout: 10_000 },
    );
    await act(async () => {
      fireEvent.click(option);
    });
  }

  async function create(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
  }

  test("lists each device with its state, keeps the order and shows the same text in the trigger", async () => {
    const dialog = await openForm(createMockTransport());
    const trigger = within(dialog).getByRole("combobox", { name: "Device" });
    await act(async () => {
      fireEvent.click(trigger);
    });
    const texts = (await screen.findAllByRole("option")).map(
      (o) => o.textContent,
    );
    expect(texts).toEqual([
      "Any device that fits",
      "build-box-1 (Online)",
      "studio-mac (Offline)",
    ]);
    await act(async () => {
      fireEvent.click(
        (screen.getAllByRole("option") as HTMLElement[]).find(
          (o) => o.textContent === "studio-mac (Offline)",
        )!,
      );
    });
    expect(trigger.textContent).toContain("studio-mac (Offline)");
  });

  test("sends the ID of a device from the second page for explicit placement", async () => {
    const organizationId = MOCK_IDS.acme;
    const mock = createMockTransport({ extraDevices: 199 });
    const client = createClient(DeviceService, mock);
    const first = await client.listDevices({ organizationId, pageSize: 200 });
    const { devices } = await client.listDevices({
      organizationId,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    const requests: CreateSandboxRequest[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "CreateSandbox") {
          requests.push(rest[3] as CreateSandboxRequest);
        }
        return mock.unary(method, ...rest);
      },
    };

    const dialog = await openForm(transport);
    await chooseDevice(dialog, "Device 201 (Online)");
    await create(dialog);

    expect(requests.map((request) => request.placement?.deviceId)).toEqual([
      devices[0]!.deviceId,
    ]);
  });

  test("locks the original request after Unavailable for a selected offline device", async () => {
    const mock = createMockTransport();
    const requests: CreateSandboxRequest[] = [];
    const transport: Transport = {
      ...mock,
      unary(method, ...rest) {
        if (method.name === "CreateSandbox") {
          requests.push(rest[3] as CreateSandboxRequest);
        }
        return mock.unary(method, ...rest);
      },
    };
    const dialog = await openForm(transport);
    await chooseDevice(dialog, "studio-mac (Offline)");
    await create(dialog);
    expect(
      await within(dialog).findByText(/The sandbox may have been created/),
    ).toBeTruthy();
    const device = within(dialog).getByRole<HTMLButtonElement>("combobox", {
      name: "Device",
    });
    expect(device.textContent).toContain("studio-mac (Offline)");
    expect(device.disabled).toBe(true);
    expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.acme}/sandboxes`);

    await act(async () => {
      fireEvent.click(
        within(dialog).getByRole("button", {
          name: "Retry original request",
        }),
      );
    });
    expect(requests).toEqual([requests[0], requests[0]]);
    expect(device.disabled).toBe(true);
  });
});
