// SPDX-License-Identifier: Apache-2.0
import { createClient, type Transport } from "@connectrpc/connect";
import {
  OrganizationService,
  type UpdateOrganizationSettingsRequest,
} from "@delino/sandrun-protos/v1/organization_pb";
import { MOCK_IDS, type MockTransportOptions } from "../src/mock/mock-api.ts";
import { createMockTransport as createCredentialMock } from "./credential-mock.ts";

const SETTINGS_FIXTURE = "sandrun-e2e-settings-limit";
const updateSettings = OrganizationService.method.updateOrganizationSettings;

declare global {
  interface Window {
    sandrunSettingsUpdates?: {
      settings: { maxDevSessionsPerUser?: number };
      paths: string[];
    }[];
  }
}

export function createMockTransport(options?: MockTransportOptions): Transport {
  const base = createCredentialMock(options);
  const initial = sessionStorage.getItem(SETTINGS_FIXTURE);
  if (initial === null) return base;

  const updates: NonNullable<Window["sandrunSettingsUpdates"]> = [];
  window.sandrunSettingsUpdates = updates;
  const seeded = createClient(
    OrganizationService,
    base,
  ).updateOrganizationSettings({
    organizationId: MOCK_IDS.globex,
    settings: { maxDevSessionsPerUser: Number(initial) },
    updateMask: { paths: ["max_dev_sessions_per_user"] },
  });
  return {
    ...base,
    async unary(method, ...args) {
      await seeded;
      if (method.name === updateSettings.name) {
        const request = args[3] as UpdateOrganizationSettingsRequest;
        const limit = request.settings?.maxDevSessionsPerUser;
        updates.push({
          settings: limit === undefined ? {} : { maxDevSessionsPerUser: limit },
          paths: request.updateMask?.paths ?? [],
        });
      }
      return base.unary(method, ...args);
    },
  };
}
