// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { Role } from "@delino/sandrun-protos/v1/common_pb";
import {
  GetOrganizationResponseSchema,
  OrganizationService,
} from "@delino/sandrun-protos/v1/organization_pb";
import {
  GetOwnSandLinkResponseSchema,
  OwnSandLinkService,
} from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { isMessage } from "@bufbuild/protobuf";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const INTEGRATION_PERMISSION_FIXTURE =
  "sandrun-e2e-integration-permission";

type IntegrationOpening = {
  opener: string;
  title: string;
  action: string;
  role: "dialog" | "alertdialog";
  mutation: string;
  row?: string;
  credential?: string;
  draftField?: string;
};

export const integrationOpenings: readonly IntegrationOpening[] = [
  {
    opener: "Link OwnSand",
    title: "Link OwnSand",
    action: "Link",
    role: "dialog",
    mutation: "SetOwnSandLink",
    credential: "OwnSand API key",
    draftField: "OwnSand organization ID",
  },
  {
    opener: "Replace API key",
    title: "Replace the API key",
    action: "Replace key",
    role: "dialog",
    mutation: "SetOwnSandLink",
    credential: "OwnSand API key",
  },
  {
    opener: "Remove link",
    title: "Remove the OwnSand link?",
    action: "Remove link",
    role: "alertdialog",
    mutation: "RemoveOwnSandLink",
  },
  {
    opener: "Change runner group",
    title: "Change the runner group",
    action: "Save",
    role: "dialog",
    mutation: "UpdateGitHubInstallation",
    row: "acme-robotics · Active",
  },
  {
    opener: "Unlink",
    title: "Unlink acme-robotics?",
    action: "Unlink",
    role: "alertdialog",
    mutation: "UnlinkGitHubInstallation",
    row: "acme-robotics · Active",
  },
  {
    opener: "Create agent integration",
    title: "Create an agent integration",
    action: "Create integration",
    role: "dialog",
    mutation: "CreateAgentIntegration",
    credential: "Environment key",
    draftField: "Name",
  },
  {
    opener: "Edit",
    title: "Edit claude-team",
    action: "Save changes",
    role: "dialog",
    mutation: "UpdateAgentIntegration",
    row: "claude-team · Claude Code",
    credential: "Environment key",
  },
  {
    opener: "Delete",
    title: "Delete claude-team?",
    action: "Delete integration",
    role: "alertdialog",
    mutation: "DeleteAgentIntegration",
    row: "claude-team · Claude Code",
  },
];

export function integrationPermissionFixture(
  base: Transport,
  unlinked: boolean,
) {
  const mutationMethods = new Set(
    integrationOpenings.map(({ mutation }) => mutation),
  );
  let role = Role.ADMIN;
  const organizationReads: Role[] = [];
  const mutations: string[] = [];
  const transport: Transport = {
    ...base,
    async unary(method, ...args) {
      if (mutationMethods.has(method.name)) {
        mutations.push(method.name);
        if (role === Role.MEMBER) {
          throw new ConnectError(
            "Fixture requires admin access",
            Code.PermissionDenied,
          );
        }
      }
      const result = await base.unary(method, ...args);
      if (
        method.name === OrganizationService.method.getOrganization.name &&
        isMessage(result.message, GetOrganizationResponseSchema) &&
        result.message.organization?.organizationId === MOCK_IDS.acme
      ) {
        result.message.organization.callerRole = role;
        organizationReads.push(role);
      }
      if (
        unlinked &&
        method.name === OwnSandLinkService.method.getOwnSandLink.name &&
        isMessage(result.message, GetOwnSandLinkResponseSchema) &&
        result.message.link?.organizationId === MOCK_IDS.acme
      ) {
        result.message.link = undefined;
      }
      return result;
    },
  };

  return {
    transport,
    organizationReads,
    mutations,
    setRole(next: Role) {
      role = next;
    },
  };
}
