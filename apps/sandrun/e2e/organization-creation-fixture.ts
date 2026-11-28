// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";

export const ORGANIZATION_CREATION_FIXTURE =
  "sandrun-e2e-organization-creation";

export function organizationCreationFixture(
  mock: Transport,
  mode: "hold-list" | "hold-create" | "fail-create",
) {
  const organizations = createClient(OrganizationService, mock);
  const state = {
    createdId: "",
    createPending: false,
    continuationPending: false,
  };
  let releaseCreate!: () => void;
  const createGate = new Promise<void>((resolve) => {
    releaseCreate = resolve;
  });
  let releaseContinuation!: () => void;
  const continuationGate = new Promise<void>((resolve) => {
    releaseContinuation = resolve;
  });
  let failNextCreate = mode === "fail-create";

  const fixture = createRouterTransport((router) => {
    router.service(OrganizationService, {
      async createOrganization(request) {
        if (failNextCreate) {
          failNextCreate = false;
          throw new ConnectError("Organization creation failed", Code.Internal);
        }
        if (mode === "hold-create") {
          state.createPending = true;
          await createGate;
          state.createPending = false;
        }
        const response = await organizations.createOrganization(request);
        state.createdId = response.organization!.organizationId;
        return response;
      },
      async listOrganizations(request, { signal }) {
        const response = await organizations.listOrganizations(
          { ...request, pageToken: "" },
          { signal },
        );
        if (mode !== "hold-list" || !state.createdId) return response;

        if (!request.pageToken) {
          return {
            organizations: response.organizations.filter(
              (organization) => organization.organizationId !== state.createdId,
            ),
            nextPageToken: "created-organization",
          };
        }
        state.continuationPending = true;
        await continuationGate;
        state.continuationPending = false;
        return {
          organizations: response.organizations.filter(
            (organization) => organization.organizationId === state.createdId,
          ),
        };
      },
    });
  });
  const transport: Transport = {
    ...mock,
    unary(method, ...args) {
      const selected =
        method.name === "CreateOrganization" ||
        method.name === "ListOrganizations"
          ? fixture
          : mock;
      return selected.unary(method, ...args);
    },
  };
  return {
    transport,
    state,
    releaseCreate,
    releaseContinuation,
  };
}
