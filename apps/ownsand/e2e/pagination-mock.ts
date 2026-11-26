// SPDX-License-Identifier: Apache-2.0
import { create, toJsonString } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import type {
  LinkBillingAccountRequest,
  ListLinkableBillingAccountsResponse,
  UnlinkBillingAccountRequest,
} from "@delino/ownsand-protos/v1/billing_pb";
import {
  MemberService,
  type AcceptInvitationResponse,
  type ListInvitationsResponse,
  type RevokeInvitationRequest,
} from "@delino/ownsand-protos/v1/member_pb";
import {
  SandboxState,
  SnapshotExportState,
} from "@delino/ownsand-protos/v1/common_pb";
import type { GetDeviceResponse } from "@delino/ownsand-protos/v1/device_pb";
import type { CreateOrganizationResponse } from "@delino/ownsand-protos/v1/organization_pb";
import {
  CreateSandboxRequestSchema,
  type CreateSandboxRequest,
  type CreateSandboxResponse,
  type ListSandboxesResponse,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import type {
  ListSnapshotsResponse,
  ListTemplatesResponse,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import {
  createMockTransport as createBaseMock,
  MOCK_IDS,
} from "../src/mock/mock-api.ts";
import { apiKeyCollisionTransport } from "./api-key-collision-fixture.ts";
import { organizationUuidTransport } from "./organization-uuid-fixture.ts";
import { browserSandboxTransitionMock } from "./sandbox-transition-mock.ts";

const lists = {
  ListApiKeys: { field: "apiKeys", firstPageSize: 1 },
  ListEnrollmentTokens: { field: "tokens", firstPageSize: 1 },
  ListMembers: { field: "members", firstPageSize: 2 },
  ListOrganizations: { field: "organizations", firstPageSize: 1 },
  ListSandboxes: { field: "sandboxes", firstPageSize: 1 },
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

function holdCredentialRequest(name: "create" | "list" | "refresh") {
  const key = `ownsand-e2e-credential-${name}`;
  if (sessionStorage.getItem(key) === "release") return Promise.resolve();
  return new Promise<void>((resolve) => {
    window.addEventListener(`${key}-release`, () => resolve(), { once: true });
    sessionStorage.setItem(key, "held");
  });
}

async function holdOrganizationRefreshStep(step: "list" | "mutation") {
  const key = `ownsand-e2e-organization-refresh-${step}`;
  if (sessionStorage.getItem(key) !== "hold") return;
  await new Promise<void>((resolve) => {
    window.addEventListener(`${key}-release`, () => resolve(), { once: true });
    sessionStorage.setItem(key, "held");
  });
  sessionStorage.setItem(key, "released");
}

function holdTemplateRequest(request: "first-page" | "create") {
  const key = `ownsand-e2e-template-${request}`;
  return new Promise<void>((resolve) => {
    window.addEventListener(`${key}-release`, () => resolve(), { once: true });
    sessionStorage.setItem(key, "held");
  });
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
  const holdTemplateFirstPage =
    sessionStorage.getItem("ownsand-e2e-template-first-page") === "hold";
  let heldTemplateFirstPage = false;
  const credentialList = sessionStorage.getItem(
    "ownsand-e2e-credential-method",
  );
  const credentialMethods = {
    ListApiKeys: {
      create: "CreateApiKey",
      rows: "apiKeys",
      secret: "apiKeySecret",
      fixture: "osk_fixture",
    },
    ListEnrollmentTokens: {
      create: "CreateEnrollmentToken",
      rows: "tokens",
      secret: "enrollmentToken",
      fixture: "ose_fixture",
    },
    ListInvitations: {
      create: "CreateInvitation",
      rows: "invitations",
      secret: "invitationSecret",
      fixture: "osi_fixture",
    },
  };
  const credential =
    credentialMethods[credentialList as keyof typeof credentialMethods];
  const holdRefresh =
    sessionStorage.getItem("ownsand-e2e-credential-hold-refresh") === "true";
  let credentialCreated = false;
  let heldRefresh = false;
  const createResponse = sessionStorage.getItem("ownsand-e2e-create-response");
  const creates: { request: string; sandboxId: string }[] = [];
  // Seed enough rows to split the selected list across two pages.
  const mock = browserSandboxTransitionMock(
    createBaseMock({
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
    }),
  );
  if (sessionStorage.getItem("ownsand-e2e-api-key-collision") === "true") {
    const continuation = "ownsand-e2e-api-key-continuation";
    const hold = sessionStorage.getItem(continuation) === "hold";
    return apiKeyCollisionTransport(mock, {
      beforeContinuation: hold
        ? async () => {
            if (sessionStorage.getItem(continuation) !== "hold") return;
            await new Promise<void>((resolve) => {
              window.addEventListener(
                `${continuation}-release`,
                () => resolve(),
                {
                  once: true,
                },
              );
              sessionStorage.setItem(continuation, "held");
            });
          }
        : undefined,
      onRevoke({ organizationId, apiKeyId }) {
        const key = "ownsand-e2e-api-key-revokes";
        const requests = JSON.parse(sessionStorage.getItem(key) ?? "[]");
        sessionStorage.setItem(
          key,
          JSON.stringify([...requests, { organizationId, apiKeyId }]),
        );
      },
    });
  }
  const base =
    sessionStorage.getItem("ownsand-e2e-organization-uuid-case") === "true"
      ? organizationUuidTransport(mock)
      : mock;
  const earlierAcceptance = createRouterTransport((router) => {
    router.rpc(MemberService.method.acceptInvitation, () => ({
      organization: { organizationId: MOCK_IDS.acme },
    }));
  });
  return {
    ...base,
    async unary(method, signal, timeout, header, input, contextValues) {
      if (holdTemplateFirstPage && method.name === "ListTemplates") {
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        // Ignore the request React Strict Mode aborts on its test mount.
        if (signal?.aborted) return response;
        const { pageToken } = input as { pageToken: string };
        recordToken("ownsand-e2e-template-list-tokens", pageToken);
        if (heldTemplateFirstPage) return response;

        heldTemplateFirstPage = true;
        // Deliver the pre-create page even after cancellation.
        await holdTemplateRequest("first-page");
        sessionStorage.setItem("ownsand-e2e-template-first-page", "delivered");
        return {
          ...response,
          message: { ...response.message, templates: [], nextPageToken: "" },
        };
      }
      if (holdTemplateFirstPage && method.name === "CreateTemplate") {
        await holdTemplateRequest("create");
        return base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
      }
      if (
        method.name === sessionStorage.getItem("ownsand-e2e-tracked-mutation")
      ) {
        recordToken("ownsand-e2e-mutation-calls", method.name);
        if (
          method.name === "CreateSandbox" ||
          method.name === "ReplaceDeviceLimits"
        ) {
          recordToken(
            "ownsand-e2e-resource-mutation-inputs",
            toJsonString(method.input, create(method.input, input), {
              alwaysEmitImplicit: true,
            }),
          );
        }
      }
      const deviceReadback = "ownsand-e2e-device-readback";
      if (
        method.name === "GetDevice" &&
        sessionStorage.getItem(deviceReadback) === "hold"
      ) {
        await new Promise<void>((resolve) => {
          window.addEventListener(
            `${deviceReadback}-release`,
            () => resolve(),
            {
              once: true,
            },
          );
          sessionStorage.setItem(deviceReadback, "held");
        });
        sessionStorage.setItem(deviceReadback, "released");
      }
      if (method.name === "GetDevice") {
        const name = sessionStorage.getItem("ownsand-e2e-device-name");
        if (name !== null) {
          const response = await base.unary(
            method,
            signal,
            timeout,
            header,
            input,
            contextValues,
          );
          const { device } = response.message as GetDeviceResponse;
          if (device?.deviceId === MOCK_IDS.buildBox) {
            return {
              ...response,
              message: { ...response.message, device: { ...device, name } },
            };
          }
          return response;
        }
      }
      const isFixturePage =
        pagination !== undefined && method.name === methodName;
      if (method.name === refreshList || isFixturePage) {
        const code = sessionStorage.getItem("ownsand-e2e-refresh-error");
        const { pageToken } = input as { pageToken?: string };
        if (code !== null && pageToken === "") {
          if (isFixturePage) recordToken("ownsand-e2e-page-tokens", pageToken);
          throw new ConnectError(
            "Synthetic full refresh failure",
            Number(code) as Code,
          );
        }
      }
      if (credential && method.name === credential.create) {
        await holdCredentialRequest("create");
        if (sessionStorage.getItem("ownsand-e2e-credential-create-fails")) {
          throw new ConnectError(
            "Synthetic create failure",
            Code.InvalidArgument,
          );
        }
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        credentialCreated = true;
        return {
          ...response,
          message: {
            ...response.message,
            [credential.secret]: credential.fixture,
          },
        };
      }
      if (credential && method.name === credentialList) {
        const afterCreate = credentialCreated;
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        // Ignore the request React Strict Mode aborts on its test mount.
        if (signal?.aborted) return response;
        const token = (input as typeof input & { pageToken: string }).pageToken;
        recordToken("ownsand-e2e-credential-tokens", token);
        if (holdRefresh) {
          if (afterCreate && !heldRefresh) {
            heldRefresh = true;
            await holdCredentialRequest("refresh");
            throw new ConnectError("Synthetic refresh failure", Code.Internal);
          }
          return response;
        }
        if (afterCreate) return response;

        // Deliver the empty pre-create page even if the client cancels it.
        await holdCredentialRequest("list");
        sessionStorage.setItem("ownsand-e2e-credential-list", "delivered");
        return {
          ...response,
          message: {
            ...response.message,
            [credential.rows]: [],
            nextPageToken: "",
          },
        };
      }
      if (method.name === "ListLinkableBillingAccounts") {
        const code = sessionStorage.getItem("ownsand-e2e-billing-list-error");
        if (code !== null) {
          throw new ConnectError(
            "Synthetic billing list failure",
            Number(code) as Code,
          );
        }
        if (
          sessionStorage.getItem("ownsand-e2e-billing-list-remove-first") ===
          "true"
        ) {
          const response = await base.unary(
            method,
            signal,
            timeout,
            header,
            input,
            contextValues,
          );
          const { billingAccounts } =
            response.message as unknown as ListLinkableBillingAccountsResponse;
          return {
            ...response,
            message: {
              ...response.message,
              billingAccounts: billingAccounts.slice(1),
            },
          };
        }
      }
      if (method.name === "UnlinkBillingAccount") {
        const request = input as UnlinkBillingAccountRequest;
        recordToken(
          "ownsand-e2e-billing-unlink-requests",
          request.expectedBillingAccountId ?? "",
        );
      }
      if (method.name === "LinkBillingAccount") {
        const { billingAccountId } =
          input as unknown as LinkBillingAccountRequest;
        recordToken("ownsand-e2e-billing-link-requests", billingAccountId);
      }
      if (method.name === "ListOrganizations") {
        await failOrganizationListIfConfigured();
        if (
          sessionStorage.getItem("ownsand-e2e-organization-refresh") === "true"
        ) {
          const { pageToken = "" } = input as { pageToken?: string };
          // Capture the rows before holding delivery. Ignore cancellation of
          // the held response to show that its late arrival cannot win.
          const response = await base.unary(
            method,
            signal,
            timeout,
            header,
            input,
            contextValues,
          );
          signal?.throwIfAborted();
          recordToken("ownsand-e2e-organization-refresh-tokens", pageToken);
          await holdOrganizationRefreshStep("list");
          return response;
        }
      }
      if (
        method.name === "CreateOrganization" ||
        method.name === "AcceptInvitation"
      ) {
        await holdOrganizationRefreshStep("mutation");
      }
      if (method.name === "CreateOrganization") {
        const reply = sessionStorage.getItem(
          "ownsand-e2e-organization-create-reply",
        );
        if (reply !== null) {
          const countKey = "ownsand-e2e-organization-create-count";
          sessionStorage.setItem(
            countKey,
            String(Number(sessionStorage.getItem(countKey)) + 1),
          );
          if (reply === "late-missing-organization") {
            await new Promise<void>((resolve) => {
              window.addEventListener(
                "ownsand-e2e-release-organization-create",
                () => resolve(),
                {
                  once: true,
                },
              );
            });
          }
          const response = await base.unary(
            method,
            signal,
            timeout,
            header,
            input,
            contextValues,
          );
          const { organization } =
            response.message as unknown as CreateOrganizationResponse;
          return {
            ...response,
            message: {
              ...response.message,
              organization:
                reply === "empty-id" && organization
                  ? { ...organization, organizationId: "" }
                  : undefined,
            },
          };
        }
      }
      if (
        method.name === "AcceptInvitation" &&
        sessionStorage.getItem("ownsand-e2e-acceptance") === "hold"
      ) {
        await new Promise<void>((resolve) => {
          window.addEventListener(
            "ownsand-e2e-release-acceptance",
            () => resolve(),
            {
              once: true,
            },
          );
          sessionStorage.setItem("ownsand-e2e-acceptance", "held");
        });
        return earlierAcceptance.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
      }
      if (method.name === "AcceptInvitation") {
        const mode = sessionStorage.getItem("ownsand-e2e-accept-response");
        if (mode !== null) {
          const count = Number(
            sessionStorage.getItem("ownsand-e2e-accept-count"),
          );
          sessionStorage.setItem("ownsand-e2e-accept-count", String(count + 1));
          const response = await base.unary(
            method,
            signal,
            timeout,
            header,
            input,
            contextValues,
          );
          if (mode === "hold") {
            await new Promise<void>((resolve) => {
              window.addEventListener(
                "ownsand-e2e-release-accept",
                () => resolve(),
                {
                  once: true,
                },
              );
              sessionStorage.setItem("ownsand-e2e-accept-held", "true");
            });
          }
          const { organization } =
            response.message as unknown as AcceptInvitationResponse;
          return {
            ...response,
            message: {
              ...response.message,
              organization:
                mode === "empty-id" && organization
                  ? { ...organization, organizationId: "" }
                  : undefined,
            },
          };
        }
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
      if (createResponse !== null && method.name === "CreateSandbox") {
        const response = await base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
        const { sandbox } =
          response.message as unknown as CreateSandboxResponse;
        if (!sandbox) throw new Error("The mock create returned no sandbox");
        creates.push({
          request: toJsonString(
            CreateSandboxRequestSchema,
            create(
              CreateSandboxRequestSchema,
              input as unknown as CreateSandboxRequest,
            ),
          ),
          sandboxId: sandbox.sandboxId,
        });
        sessionStorage.setItem("ownsand-e2e-creates", JSON.stringify(creates));
        if (
          (createResponse === "lost" && creates.length === 1) ||
          (createResponse === "lost-twice" && creates.length <= 2)
        ) {
          throw new ConnectError("Response lost", Code.Unavailable);
        }
        if (creates.length > 1 || createResponse === "valid") return response;
        if (createResponse === "held") {
          await new Promise<void>((resolve) => {
            window.addEventListener(
              "ownsand-e2e-release-create",
              () => resolve(),
              { once: true },
            );
          });
        }
        return {
          ...response,
          message: {
            ...response.message,
            sandbox:
              createResponse === "empty-id"
                ? { ...sandbox, sandboxId: "" }
                : undefined,
          },
        };
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
      const nextPageToken =
        sessionStorage.getItem("ownsand-e2e-paged-next-token") ?? CONTINUATION;
      if (token !== "") {
        if (pagination !== undefined && token !== nextPageToken) {
          throw new ConnectError(
            "Unexpected continuation token",
            Code.InvalidArgument,
          );
        }
        await failContinuationIfConfigured();
      } else if (
        methodName === "ListSandboxes" &&
        sessionStorage.getItem("ownsand-e2e-sandbox-first-page-error") ===
          "true"
      ) {
        throw new ConnectError("Synthetic first page failure", Code.Internal);
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
      if (
        methodName === "ListSandboxes" &&
        sessionStorage.getItem("ownsand-e2e-sandbox-creating") === "true"
      ) {
        const { sandboxes } = response.message as ListSandboxesResponse;
        sandboxes[0]!.state = SandboxState.CREATING;
      }
      const rows = (response.message as unknown as Record<string, unknown[]>)[
        field
      ]!;
      const message =
        token === ""
          ? {
              ...response.message,
              [field]: rows.slice(0, firstPageSize),
              nextPageToken,
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
