// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { createClient, type Transport } from "@connectrpc/connect";
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

const VCPU_ERROR = "vCPU limit must be a whole number from 0 to 2147483647.";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  toast.dismiss();
});

/** A mock control plane whose build box has 16 vCPUs, and that records saves. */
async function recordingTransport() {
  const mock = createMockTransport();
  await createClient(DeviceService, mock).replaceDeviceLimits({
    organizationId: MOCK_IDS.acme,
    deviceId: MOCK_IDS.buildBox,
    limits: {
      vcpuCount: 16,
      memoryMib: 65536n,
      diskGib: 500n,
      vcpuOvercommitPercent: 150,
    },
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

const vcpuInput = () => screen.getByLabelText("vCPU limit") as HTMLInputElement;

async function commit(value: string) {
  await act(async () => {
    fireEvent.input(vcpuInput(), { target: { value } });
    fireEvent.blur(vcpuInput());
  });
}

async function save() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
  });
}

describe("device vCPU limit", () => {
  test("an out-of-range vCPU limit shows a field error and sends nothing", async () => {
    const { transport, replaced } = await recordingTransport();
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    await waitFor(() => expect(vcpuInput().value).toBe("16"), WAIT);
    for (const value of ["2147483648", "99999999999"]) {
      await commit(value);
      await save();
      await waitFor(
        () => expect(vcpuInput().getAttribute("aria-invalid")).toBe("true"),
        WAIT,
      );
      // The message shows on the field and in the toast.
      await waitFor(
        () => expect(screen.getAllByText(VCPU_ERROR).length).toBeGreaterThan(1),
        WAIT,
      );
      toast.dismiss();
    }
    expect(replaced).toHaveLength(0);
    // Editing the field clears its error.
    await commit("2147483647");
    await waitFor(
      () => expect(vcpuInput().getAttribute("aria-invalid")).not.toBe("true"),
      WAIT,
    );
  });

  test("the bounds 2147483647 and 0 save, and a blank value asks for the limit", async () => {
    const { transport, replaced } = await recordingTransport();
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${MOCK_IDS.buildBox}`, {
      transport,
    });
    await waitFor(() => expect(vcpuInput().value).toBe("16"), WAIT);
    await commit("");
    await save();
    await waitFor(
      () =>
        expect(
          screen.getAllByText("vCPU limit is required. Enter 0 to allow none.")
            .length,
        ).toBeGreaterThan(0),
      WAIT,
    );
    expect(replaced).toHaveLength(0);
    await commit("2147483647");
    await save();
    await waitFor(() => expect(replaced).toHaveLength(1), WAIT);
    expect(replaced[0]?.vcpuCount).toBe(2147483647);
    await commit("0");
    await save();
    await waitFor(() => expect(replaced).toHaveLength(2), WAIT);
    expect(replaced[1]?.vcpuCount).toBe(0);
  });
});
