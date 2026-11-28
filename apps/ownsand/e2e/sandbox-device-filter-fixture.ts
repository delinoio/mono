// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import {
  DeviceService,
  type ListDevicesRequest,
  type ListDevicesResponse,
} from "@delino/ownsand-protos/v1/device_pb";
import type { ListSandboxesRequest } from "@delino/ownsand-protos/v1/sandbox_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

declare global {
  interface Window {
    sandboxDeviceFilterFixture: {
      removeDevice(): Promise<void>;
      releaseFirstPage(): void;
      failContinuation(): void;
      allowRetry(): void;
      refreshingAfterRemoval: boolean;
      inventoryTokens: string[];
      sandboxRequests: Pick<
        ListSandboxesRequest,
        "deviceId" | "includeEnded"
      >[];
    };
  }
}

function holdResponse() {
  let release!: () => void;
  const pending = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { pending, release };
}

export function sandboxDeviceFilterTransport(): Transport {
  // Once the selected device is removed, 200 devices remain. The mock
  // returns a cursor for this full first page and ends on the empty page.
  const mock = createMockTransport({ extraDevices: 199 });
  const devices = createClient(DeviceService, mock);
  const firstPage = holdResponse();
  const continuation = holdResponse();
  let removed = false;
  let retryAllowed = false;
  const inventoryTokens: string[] = [];
  const sandboxRequests: Window["sandboxDeviceFilterFixture"]["sandboxRequests"] =
    [];
  window.sandboxDeviceFilterFixture = {
    async removeDevice() {
      await devices.removeDevice({
        organizationId: MOCK_IDS.acme,
        deviceId: MOCK_IDS.buildBox,
      });
      removed = true;
    },
    releaseFirstPage: firstPage.release,
    failContinuation: continuation.release,
    allowRetry() {
      retryAllowed = true;
    },
    refreshingAfterRemoval: false,
    inventoryTokens,
    sandboxRequests,
  };
  return {
    ...mock,
    async unary(method, ...args) {
      if (method.name === "ListSandboxes") {
        const { deviceId, includeEnded } = args[3] as ListSandboxesRequest;
        sandboxRequests.push({ deviceId, includeEnded });
      }
      if (method.name !== "ListDevices") return mock.unary(method, ...args);

      const { pageToken } = args[3] as ListDevicesRequest;
      inventoryTokens.push(pageToken);
      if (!removed) {
        // The initial inventory fits on one page. The fixture exposes the
        // rest of the seeded inventory when the other client removes a device.
        const response = await mock.unary(method, ...args);
        const { devices } = response.message as ListDevicesResponse;
        return {
          ...response,
          message: {
            ...response.message,
            devices: devices.filter(
              (device) =>
                device.deviceId === MOCK_IDS.buildBox ||
                device.deviceId === MOCK_IDS.studioMac,
            ),
            nextPageToken: "",
          },
        };
      }
      if (pageToken === "") {
        window.sandboxDeviceFilterFixture.refreshingAfterRemoval = true;
        await firstPage.pending;
      } else if (!retryAllowed) {
        await continuation.pending;
        throw new ConnectError(
          "Inventory continuation failed",
          Code.Unavailable,
        );
      }
      return mock.unary(method, ...args);
    },
  };
}
