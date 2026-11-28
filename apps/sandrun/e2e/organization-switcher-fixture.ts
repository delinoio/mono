// SPDX-License-Identifier: Apache-2.0
import { isMessage } from "@bufbuild/protobuf";
import type { Transport } from "@connectrpc/connect";
import {
  GetOrganizationResponseSchema,
  ListOrganizationsResponseSchema,
  type Organization,
} from "@delino/sandrun-protos/v1/organization_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const ORGANIZATION_SWITCHER_FIXTURE =
  "sandrun-e2e-organization-switcher";
export const ACME_UUID_SPELLINGS = {
  lowercase: "0193b00c-0000-7000-8000-000000000001",
  mixed: "0193B00c-0000-7000-8000-000000000001",
  uppercase: "0193B00C-0000-7000-8000-000000000001",
};

export function caseInsensitiveAcmeTransport(base: Transport): Transport {
  function useFixtureId(organization: Organization) {
    if (organization.organizationId === MOCK_IDS.acme) {
      organization.organizationId = ACME_UUID_SPELLINGS.lowercase;
    }
  }

  return {
    ...base,
    async unary(method, signal, timeoutMs, header, input, contextValues) {
      // Give the seeded Acme UUID two hexadecimal letters for the mixed-case
      // route. Resolve every spelling to the ID the mock uses for membership.
      const { organizationId } = input as { organizationId?: string };
      const request =
        organizationId?.toLowerCase() === ACME_UUID_SPELLINGS.lowercase
          ? { ...input, organizationId: MOCK_IDS.acme }
          : input;
      const response = await base.unary(
        method,
        signal,
        timeoutMs,
        header,
        request,
        contextValues,
      );
      const message = response.message;
      if (
        isMessage(message, GetOrganizationResponseSchema) &&
        message.organization
      ) {
        useFixtureId(message.organization);
      } else if (isMessage(message, ListOrganizationsResponseSchema)) {
        message.organizations.forEach(useFixtureId);
      }
      return response;
    },
  };
}
