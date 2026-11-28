// SPDX-License-Identifier: Apache-2.0
import { isMessage } from "@bufbuild/protobuf";
import {
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { RequestState, Role } from "@delino/sandrun-protos/v1/common_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import { GetOrganizationResponseSchema } from "@delino/sandrun-protos/v1/organization_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const WORK_DIALOG_PERMISSION_FIXTURE =
  "sandrun-e2e-work-dialog-permission";

type Opening = {
  page: string;
  row: string;
  rowRole: "row" | "listitem";
  opener: string;
  role: "dialog" | "alertdialog";
  title: string;
  action: string;
  draftDefault?: string;
};

const otherSessionDelete: Opening = {
  page: "dev-sessions",
  row: "compiler check",
  rowRole: "row",
  opener: "Delete",
  role: "alertdialog",
  title: "Delete this dev session?",
  action: "Delete dev session",
};

export const ownSessionDelete: Opening = {
  ...otherSessionDelete,
  row: "msvc build",
};

export const editProfile: Opening = {
  page: "profiles",
  row: "linux-build",
  rowRole: "listitem",
  opener: "Edit",
  role: "dialog",
  title: "Edit linux-build",
  action: "Save changes",
  draftDefault: "linux-build",
};

export const workDialogOpenings: readonly Opening[] = [
  {
    page: "requests",
    row: "0193b000…00e1",
    rowRole: "row",
    opener: "Cancel",
    role: "alertdialog",
    title: "Cancel this request?",
    action: "Cancel request",
  },
  {
    page: "runners",
    row: "0193b000…00f2",
    rowRole: "row",
    opener: "Delete",
    role: "alertdialog",
    title: "Delete this runner?",
    action: "Delete runner",
  },
  {
    page: "profiles",
    row: "linux-build",
    rowRole: "listitem",
    opener: "Create runner profile",
    role: "dialog",
    title: "Create a runner profile",
    action: "Create profile",
    draftDefault: "",
  },
  editProfile,
  {
    page: "profiles",
    row: "linux-build",
    rowRole: "listitem",
    opener: "Delete",
    role: "alertdialog",
    title: "Delete linux-build?",
    action: "Delete profile",
  },
  otherSessionDelete,
];

export function workDialogPermissionFixture(base: Transport) {
  const mutationMethods = new Set([
    "CancelRequest",
    "DeleteRunner",
    "CreateRunnerProfile",
    "UpdateRunnerProfile",
    "DeleteRunnerProfile",
    "DeleteDevSession",
  ]);
  const sessions = createClient(DevSessionService, base);
  const sessionFixture = createRouterTransport((router) => {
    router.service(DevSessionService, {
      async listDevSessions(request) {
        const response = await sessions.listDevSessions(request);
        if (request.organizationId !== MOCK_IDS.acme) return response;
        const withEnded = request.includeEnded
          ? response
          : await sessions.listDevSessions({ ...request, includeEnded: true });
        const other = withEnded.devSessions.find(
          (session) => session.devSessionId === MOCK_IDS.devSessionOther,
        );
        if (other !== undefined) {
          other.state = RequestState.QUEUED;
          if (!request.includeEnded) response.devSessions.push(other);
        }
        return response;
      },
    });
  });
  let role = Role.ADMIN;
  const organizationReads: Role[] = [];
  const mutations: { method: string; input: unknown }[] = [];
  const transport: Transport = {
    ...base,
    async unary(method, ...args) {
      if (mutationMethods.has(method.name)) {
        mutations.push({ method: method.name, input: args[3] });
      }
      if (method.name === DevSessionService.method.listDevSessions.name) {
        return sessionFixture.unary(method, ...args);
      }
      const result = await base.unary(method, ...args);
      if (
        isMessage(result.message, GetOrganizationResponseSchema) &&
        result.message.organization?.organizationId === MOCK_IDS.acme
      ) {
        result.message.organization.callerRole = role;
        organizationReads.push(role);
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
