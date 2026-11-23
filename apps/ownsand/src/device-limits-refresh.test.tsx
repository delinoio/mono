// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { QueryClient } from "@tanstack/react-query";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { type DeviceLimitsInput } from "@delino/ownsand-protos/v1/common_pb";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { createMockTransport, MOCK_IDS } from "./mock/mock-api.ts";
import { renderConsole } from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

const BUILD_BOX = `/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`;
const STUDIO_MAC = `/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.studioMac}`;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  toast.dismiss();
});

interface Limits {
  vcpuCount: number;
  memoryMib: bigint;
  diskGib: bigint;
  vcpuOvercommitPercent: number;
}

/**
 * A control plane whose devices have the given limits. `serverChange` sets
 * the limits of a device the way another session would, behind the page's
 * back, and `saves` records the requests of the page. While `failSaves` is
 * set, ReplaceDeviceLimits fails with `unavailable`.
 */
async function controlPlane(initial: Limits) {
  const mock = createMockTransport();
  const server = createClient(DeviceService, mock);
  const serverChange = async (deviceId: string, limits: Limits) => {
    await server.replaceDeviceLimits({
      organizationId: MOCK_IDS.acme,
      deviceId,
      limits,
    });
  };
  await serverChange(MOCK_IDS.buildBox, initial);
  const state = { failSaves: false, saves: [] as DeviceLimitsInput[] };
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      if (method.name === "ReplaceDeviceLimits") {
        state.saves.push((rest[3] as { limits: DeviceLimitsInput }).limits);
        if (state.failSaves) {
          throw new ConnectError("the save failed", Code.Unavailable);
        }
      }
      return mock.unary(method, ...rest);
    },
  };
  return { transport, serverChange, state };
}

function renderDevice(path: string, transport: Transport) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  renderConsole(path, { transport, queryClient });
  return queryClient;
}

const input = (label: string) =>
  screen.getByLabelText(label) as HTMLInputElement;

/** Waits until the form shows the limits, before any edit. */
async function loaded(vcpu: string, memory: string, disk: string) {
  await waitFor(() => {
    expect(input("vCPU limit").value).toBe(vcpu);
    expect(input("Memory limit (MiB)").value).toBe(memory);
    expect(input("Disk limit (GiB)").value).toBe(disk);
  }, WAIT);
}

async function commit(label: string, value: string) {
  await act(async () => {
    fireEvent.input(input(label), { target: { value } });
    fireEvent.blur(input(label));
  });
}

const effective = () =>
  screen.getByRole("status", { name: "Effective vCPU limit" }).textContent;

/** Refetches the device as a later poll or a focus refetch of the page would. */
async function refresh(queryClient: QueryClient, expected: () => void) {
  await act(async () => {
    await queryClient.invalidateQueries();
  });
  await waitFor(expected, WAIT);
}

async function save() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
  });
}

const FIRST: Limits = {
  vcpuCount: 4,
  memoryMib: 4096n,
  diskGib: 20n,
  vcpuOvercommitPercent: 150,
};
const CHANGED: Limits = {
  vcpuCount: 6,
  memoryMib: 6144n,
  diskGib: 30n,
  vcpuOvercommitPercent: 125,
};

describe("device limits during a refresh", () => {
  test("a refresh keeps the edited field, updates the others and saves all four", async () => {
    const { transport, serverChange, state } = await controlPlane(FIRST);
    const queryClient = renderDevice(BUILD_BOX, transport);
    await loaded("4", "4096", "20");
    await commit("Memory limit (MiB)", "8192");
    await serverChange(MOCK_IDS.buildBox, CHANGED);
    await refresh(queryClient, () => {
      expect(input("vCPU limit").value).toBe("6");
      expect(input("Disk limit (GiB)").value).toBe("30");
    });
    expect(input("Memory limit (MiB)").value).toBe("8192");
    // The untouched overcommit select follows the fetched 125%: 6 * 1.25.
    expect(effective()).toContain("7");

    await save();
    await waitFor(() => expect(state.saves).toHaveLength(1), WAIT);
    expect(state.saves[0]?.vcpuCount).toBe(6);
    expect(state.saves[0]?.memoryMib).toBe(8192n);
    expect(state.saves[0]?.diskGib).toBe(30n);
    expect(state.saves[0]?.vcpuOvercommitPercent).toBe(125);
  });

  test("a saved edit is cleared, so a later change of the server updates the field", async () => {
    const { transport, serverChange, state } = await controlPlane(FIRST);
    const queryClient = renderDevice(BUILD_BOX, transport);
    await loaded("4", "4096", "20");
    await commit("Memory limit (MiB)", "8192");
    await save();
    await waitFor(() => expect(state.saves).toHaveLength(1), WAIT);
    await waitFor(() => {
      expect(
        (
          screen.getByRole("button", {
            name: "Save limits",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false);
    }, WAIT);
    expect(input("Memory limit (MiB)").value).toBe("8192");

    await serverChange(MOCK_IDS.buildBox, CHANGED);
    await refresh(queryClient, () => {
      expect(input("Memory limit (MiB)").value).toBe("6144");
    });
    expect(input("vCPU limit").value).toBe("6");
  });

  test("a failed save keeps the draft for a retry", async () => {
    const { transport, serverChange, state } = await controlPlane(FIRST);
    const queryClient = renderDevice(BUILD_BOX, transport);
    await loaded("4", "4096", "20");
    await commit("Disk limit (GiB)", "99");
    state.failSaves = true;
    await save();
    await waitFor(() => expect(state.saves).toHaveLength(1), WAIT);
    await waitFor(() => {
      expect(
        (
          screen.getByRole("button", {
            name: "Save limits",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false);
    }, WAIT);
    expect(input("Disk limit (GiB)").value).toBe("99");

    // A refresh after the failure still keeps the edit.
    await serverChange(MOCK_IDS.buildBox, CHANGED);
    await refresh(queryClient, () => {
      expect(input("vCPU limit").value).toBe("6");
    });
    expect(input("Disk limit (GiB)").value).toBe("99");

    state.failSaves = false;
    await save();
    await waitFor(() => expect(state.saves).toHaveLength(2), WAIT);
    expect(state.saves[1]?.vcpuCount).toBe(6);
    expect(state.saves[1]?.diskGib).toBe(99n);
    expect(state.saves[1]?.memoryMib).toBe(6144n);
  });

  test("a clean form shows the latest limits and another device shows its own", async () => {
    const { transport, serverChange } = await controlPlane(FIRST);
    await serverChange(MOCK_IDS.studioMac, {
      vcpuCount: 2,
      memoryMib: 2048n,
      diskGib: 10n,
      vcpuOvercommitPercent: 100,
    });
    const queryClient = renderDevice(BUILD_BOX, transport);
    await loaded("4", "4096", "20");
    await serverChange(MOCK_IDS.buildBox, CHANGED);
    await refresh(queryClient, () => {
      expect(input("Memory limit (MiB)").value).toBe("6144");
    });
    expect(input("vCPU limit").value).toBe("6");
    expect(input("Disk limit (GiB)").value).toBe("30");

    // An edit of this device does not follow the user to the other one.
    await commit("Disk limit (GiB)", "77");
    await act(async () => {
      window.history.pushState(null, "", STUDIO_MAC);
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await loaded("2", "2048", "10");
  });
});
