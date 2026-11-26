// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import type { QueryClient } from "@tanstack/react-query";
import { DeviceState } from "@delino/ownsand-protos/v1/common_pb";
import {
  DeviceService,
  type ListDevicesResponse,
} from "@delino/ownsand-protos/v1/device_pb";
import type { CreateSandboxRequest } from "@delino/ownsand-protos/v1/sandbox_pb";
import { deviceOptionLabel, errorMessage } from "./format.ts";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import {
  fullListRefresh,
  holdingTransport,
  DEFAULT_WAIT_MS,
  TEST_SETUP_MS,
  renderConsole,
  settle,
} from "./test-support.tsx";

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

const DEVICE_OPTION_WAIT_MS = 10_000;

describe("create sandbox device select", () => {
  async function openForm(transport: Transport, queryClient?: QueryClient) {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport,
      queryClient,
    });
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
      { timeout: DEVICE_OPTION_WAIT_MS },
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

  test(
    "sends the ID of a device from the second page for explicit placement",
    async () => {
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
    },
    DEVICE_OPTION_WAIT_MS + 2 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  function deviceFixture(mock = createMockTransport()) {
    let omitSelected = false;
    let failContinuation = false;
    const requests: CreateSandboxRequest[] = [];
    const refresh = fullListRefresh(DeviceService.method.listDevices, {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "CreateSandbox") {
          requests.push(rest[3] as CreateSandboxRequest);
        }
        if (
          method.name === "ListDevices" &&
          failContinuation &&
          (rest[3] as { pageToken: string }).pageToken !== ""
        ) {
          throw new ConnectError("Continuation failed", Code.Unavailable);
        }
        const response = await mock.unary(method, ...rest);
        if (method.name === "ListDevices" && omitSelected) {
          const { devices } = response.message as ListDevicesResponse;
          return {
            ...response,
            message: {
              ...response.message,
              devices: devices.filter(
                (device) => device.deviceId !== MOCK_IDS.buildBox,
              ),
            },
          };
        }
        return response;
      },
    });
    return {
      ...refresh,
      requests,
      omitSelected() {
        omitSelected = true;
        return refresh.refetch();
      },
      failContinuation() {
        failContinuation = true;
      },
    };
  }

  const unavailable = /The selected device is unavailable/;
  const permissionDenied = "You do not have permission to do this.";

  for (const replacement of ["Any device that fits", "Device 003 (Online)"]) {
    test(`blocks a missing selection until the user chooses ${replacement}`, async () => {
      const mock = createMockTransport({ extraDevices: 1 });
      const { devices } = await createClient(DeviceService, mock).listDevices({
        organizationId: MOCK_IDS.acme,
      });
      const deviceId =
        replacement === "Any device that fits"
          ? undefined
          : devices.find((device) => device.name === "Device 003")!.deviceId;
      const fixture = deviceFixture(mock);
      const dialog = await openForm(fixture.transport, fixture.queryClient);
      await chooseDevice(dialog, "build-box-1 (Online)");
      const form = within(dialog);
      const label = form.getByRole<HTMLTextAreaElement>("textbox", {
        name: "Labels",
      });
      await act(async () => {
        fireEvent.input(label, { target: { value: "team=placement" } });
      });

      await fixture.omitSelected();
      await form.findByText(unavailable);
      expect(
        form.getByRole<HTMLButtonElement>("button", { name: "Create" })
          .disabled,
      ).toBe(true);
      await act(async () => {
        fireEvent.submit(label.form!);
        fireEvent.click(form.getByRole("combobox", { name: "Device" }));
      });
      expect(
        screen.queryByRole("option", { name: "build-box-1 (Online)" }),
      ).toBeNull();
      expect(fixture.requests).toEqual([]);

      await act(async () => {
        fireEvent.click(screen.getByRole("option", { name: replacement }));
      });
      expect(form.queryByText(unavailable)).toBeNull();
      await create(dialog);

      expect(fixture.requests).toHaveLength(1);
      expect(fixture.requests[0]?.placement?.deviceId).toBe(deviceId);
      expect(fixture.requests[0]?.labels).toEqual({ team: "placement" });
    });
  }

  test("restores a selection when the device list recovers from a terminal error", async () => {
    const fixture = deviceFixture();
    const dialog = await openForm(fixture.transport, fixture.queryClient);
    await chooseDevice(dialog, "build-box-1 (Online)");
    const form = within(dialog);
    const submit = form.getByRole<HTMLButtonElement>("button", {
      name: "Create",
    });
    const label = form.getByRole<HTMLTextAreaElement>("textbox", {
      name: "Labels",
    });
    await act(async () => {
      fireEvent.input(label, { target: { value: "team=recovery" } });
    });

    await fixture.refetch(Code.PermissionDenied);
    await form.findByText(permissionDenied);
    expect(form.getByText(unavailable)).toBeTruthy();
    expect(submit.disabled).toBe(true);
    await act(async () => {
      fireEvent.submit(label.form!);
    });
    expect(fixture.requests).toEqual([]);

    await fixture.refetch();
    await waitFor(() => expect(form.queryByText(unavailable)).toBeNull());
    expect(
      form.getByRole("combobox", { name: "Device" }).textContent,
    ).toContain("build-box-1 (Online)");
    expect(submit.disabled).toBe(false);
    await create(dialog);
    expect(fixture.requests).toHaveLength(1);
    expect(fixture.requests[0]?.placement?.deviceId).toBe(MOCK_IDS.buildBox);
    expect(fixture.requests[0]?.labels).toEqual({ team: "recovery" });
  });

  test("keeps a visible device usable when a later page fails", async () => {
    const fixture = deviceFixture(createMockTransport({ extraDevices: 199 }));
    fixture.failContinuation();
    const dialog = await openForm(fixture.transport, fixture.queryClient);
    await within(dialog).findByText("Some devices are not shown yet.");
    await chooseDevice(dialog, "build-box-1 (Online)");
    await create(dialog);
    expect(
      fixture.requests.map((request) => request.placement?.deviceId),
    ).toEqual([MOCK_IDS.buildBox]);
  });

  test("replays a submitted request with its original device and key after a terminal list error", async () => {
    let hold = true;
    const held = holdingTransport(["CreateSandbox"], () => hold);
    const fixture = deviceFixture(held.transport);
    const dialog = await openForm(fixture.transport, fixture.queryClient);
    await chooseDevice(dialog, "build-box-1 (Online)");
    await create(dialog);

    await fixture.refetch(Code.PermissionDenied);
    await within(dialog).findByText(permissionDenied);
    expect(within(dialog).queryByText(unavailable)).toBeNull();
    expect(
      within(dialog).getByRole("combobox", { name: "Device" }).textContent,
    ).toContain("build-box-1 (Online)");
    const [original] = held.calls.CreateSandbox!;
    await act(async () => {
      original!.settle(new ConnectError("Lost reply", Code.Unavailable));
    });
    hold = false;

    const retry = await within(dialog).findByRole<HTMLButtonElement>("button", {
      name: "Retry original request",
    });
    expect(retry.disabled).toBe(false);
    await act(async () => {
      fireEvent.click(retry);
    });
    expect(fixture.requests).toEqual([
      fixture.requests[0],
      fixture.requests[0],
    ]);
    expect(fixture.requests[0]?.placement?.deviceId).toBe(MOCK_IDS.buildBox);
  });

  test(
    "locks the original request after Unavailable for a selected offline device",
    async () => {
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
    },
    DEVICE_OPTION_WAIT_MS + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );
});
