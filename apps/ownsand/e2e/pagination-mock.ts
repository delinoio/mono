// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import type {
  ListInvitationsResponse,
  RevokeInvitationRequest,
} from "@delino/ownsand-protos/v1/member_pb";
import { SnapshotExportState } from "@delino/ownsand-protos/v1/common_pb";
import type { CreateSandboxRequest } from "@delino/ownsand-protos/v1/sandbox_pb";
import type {
  ListSnapshotsResponse,
  ListTemplatesResponse,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import { createMockTransport as createBaseMock } from "../src/mock/mock-api.ts";

const lists = {
  ListApiKeys: { field: "apiKeys", firstPageSize: 1 },
  ListEnrollmentTokens: { field: "tokens", firstPageSize: 1 },
  ListMembers: { field: "members", firstPageSize: 2 },
  ListOrganizations: { field: "organizations", firstPageSize: 1 },
};

const CONTINUATION = "e2e-next-page";

async function failOrganizationListIfConfigured() {
  if (
    sessionStorage.getItem("ownsand-e2e-organization-list-error") === "hold"
  ) {
    await new Promise<void>((resolve) => {
      window.addEventListener(
        "ownsand-e2e-release-organization-list",
        () => resolve(),
        { once: true },
      );
    });
  }
  const code = sessionStorage.getItem("ownsand-e2e-organization-list-error");
  if (code !== null) {
    throw new ConnectError(
      "Synthetic organization list failure",
      Number(code) as Code,
    );
  }
}

async function failContinuationIfConfigured() {
  if (sessionStorage.getItem("ownsand-e2e-page-error") === "hold") {
    await new Promise<void>((resolve) => {
      window.addEventListener("ownsand-e2e-release-page", () => resolve(), {
        once: true,
      });
    });
  }
  const code = sessionStorage.getItem("ownsand-e2e-page-error");
  if (code !== null) {
    throw new ConnectError(
      "Synthetic continuation failure",
      Number(code) as Code,
    );
  }
}

function recordToken(key: string, token: string) {
  const tokens = JSON.parse(sessionStorage.getItem(key) ?? "[]") as string[];
  sessionStorage.setItem(key, JSON.stringify([...tokens, token]));
}

export function createMockTransport(): Transport {
  const methodName = sessionStorage.getItem("ownsand-e2e-paged-list");
  const refreshList = sessionStorage.getItem("ownsand-e2e-refresh-list");
  const pagination =
    methodName !== null && Object.hasOwn(lists, methodName)
      ? lists[methodName as keyof typeof lists]
      : undefined;
  // These lists use the base mock's control-plane page size and cursor.
  const pageDevices = methodName === "ListDevices";
  const pageInvitations = methodName === "ListInvitations";
  const pageSources = methodName === "SnapshotSources";
  // Seed enough rows to split the selected list across two pages.
  const base = createBaseMock({
    extraApiKeys: Number(methodName === "ListApiKeys"),
    extraDevices: pageDevices ? 199 : 0,
    extraEnrollmentTokens: Number(
      methodName === "ListEnrollmentTokens" ||
        refreshList === "ListEnrollmentTokens",
    ),
    extraInvitations: pageInvitations
      ? Number(sessionStorage.getItem("ownsand-e2e-extra-invitations") ?? 200)
      : 0,
    extraSnapshots: pageSources ? 200 : 0,
    extraTemplates: pageSources ? 200 : 0,
  });
  return {
    ...base,
    async unary(method, signal, timeout, header, input, contextValues) {
      if (
        method.name === sessionStorage.getItem("ownsand-e2e-tracked-mutation")
      ) {
        recordToken("ownsand-e2e-mutation-calls", method.name);
      }
      if (method.name === refreshList) {
        const code = sessionStorage.getItem("ownsand-e2e-refresh-error");
        const { pageToken } = input as { pageToken?: string };
        if (code !== null && pageToken === "") {
          throw new ConnectError(
            "Synthetic full refresh failure",
            Number(code) as Code,
          );
        }
      }
      if (method.name === "ListOrganizations") {
        await failOrganizationListIfConfigured();
      }
      if (
        pageInvitations &&
        method.name === "CreateInvitation" &&
        sessionStorage.getItem("ownsand-e2e-hold-invitation") === "true"
      ) {
        await new Promise<void>((resolve) => {
          window.addEventListener(
            "ownsand-e2e-release-invitation",
            () => resolve(),
            { once: true },
          );
          sessionStorage.setItem("ownsand-e2e-invitation-held", "true");
        });
      }
      if (pageInvitations && method.name === "RevokeInvitation") {
        const { organizationId, invitationId } =
          input as unknown as RevokeInvitationRequest;
        sessionStorage.setItem(
          "ownsand-e2e-revoked-invitation",
          JSON.stringify({ organizationId, invitationId }),
        );
      }
      if (pageSources && method.name === "CreateSandbox") {
        const { source } = input as unknown as CreateSandboxRequest;
        sessionStorage.setItem(
          "ownsand-e2e-created-source",
          JSON.stringify(source?.source),
        );
      }
      if (
        pageSources &&
        (method.name === "ListSnapshots" || method.name === "ListTemplates")
      ) {
        const { pageToken = "" } = input as { pageToken?: string };
        recordToken(`ownsand-e2e-${method.name}-tokens`, pageToken);
        if (
          pageToken !== "" &&
          sessionStorage.getItem("ownsand-e2e-source-fault") === method.name
        ) {
          await failContinuationIfConfigured();
        }
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        if (pageToken === "") return response;
        if (method.name === "ListTemplates") {
          const { templates } =
            response.message as unknown as ListTemplatesResponse;
          const template = templates[0];
          if (template)
            sessionStorage.setItem(
              "ownsand-e2e-later-template",
              template.templateId,
            );
          return response;
        }
        const { snapshots } =
          response.message as unknown as ListSnapshotsResponse;
        const snapshot = snapshots[0];
        if (!snapshot) return response;
        sessionStorage.setItem(
          "ownsand-e2e-later-snapshot",
          snapshot.snapshotId,
        );
        if (
          sessionStorage.getItem("ownsand-e2e-snapshot-exporting") !== "true"
        ) {
          return response;
        }
        return {
          ...response,
          message: {
            ...response.message,
            snapshots: [
              { ...snapshot, exportState: SnapshotExportState.EXPORTING },
              ...snapshots.slice(1),
            ],
          },
        };
      }
      if (
        method.name !== methodName ||
        (!pageDevices && !pageInvitations && pagination === undefined)
      ) {
        return base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
      }
      const request = input as typeof input & { pageToken?: string };
      const token = request.pageToken ?? "";
      recordToken("ownsand-e2e-page-tokens", token);
      if (token !== "") {
        if (pagination !== undefined && token !== CONTINUATION) {
          throw new ConnectError(
            "Unexpected continuation token",
            Code.InvalidArgument,
          );
        }
        await failContinuationIfConfigured();
      }

      if (pagination === undefined) {
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        if (pageInvitations && token !== "") {
          const { invitations } =
            response.message as unknown as ListInvitationsResponse;
          const later = invitations[0];
          if (later) {
            sessionStorage.setItem(
              "ownsand-e2e-later-invitation",
              later.invitationId,
            );
          }
        }
        return response;
      }
      const { field, firstPageSize } = pagination;
      const response = await base.unary(
        method,
        signal,
        timeout,
        header,
        { ...request, pageToken: "" },
        contextValues,
      );
      const rows = (response.message as unknown as Record<string, unknown[]>)[
        field
      ]!;
      const message =
        token === ""
          ? {
              ...response.message,
              [field]: rows.slice(0, firstPageSize),
              nextPageToken: CONTINUATION,
            }
          : {
              ...response.message,
              [field]: rows.slice(firstPageSize),
              nextPageToken: "",
            };
      return { ...response, message };
    },
  };
}
