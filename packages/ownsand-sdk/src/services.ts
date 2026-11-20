// SPDX-License-Identifier: Apache-2.0
import type { DescMethod, DescService } from "@bufbuild/protobuf";
import {
  type CallOptions,
  type Client,
  Code,
  createClient,
  type Transport,
} from "@connectrpc/connect";

import { localError } from "./errors.ts";

/** RPCs of the public API that take no `organization_id`. */
const ORGANIZATION_FREE = new Set([
  "delino.ownsand.v1.OrganizationService.CreateOrganization",
  "delino.ownsand.v1.OrganizationService.ListOrganizations",
  "delino.ownsand.v1.MemberService.AcceptInvitation",
]);

type UnaryMethods<S extends DescService> = {
  [
    K in keyof Client<S> as Client<S>[K] extends (
      ...args: never[]
    ) => Promise<unknown>
      ? K
      : never
  ]: Client<S>[K];
};

/**
 * A Connect client of one public service whose requests may omit
 * `organizationId`; the client then fills in its default organization.
 */
export type OrgScopedClient<S extends DescService> = {
  [K in keyof UnaryMethods<S>]: UnaryMethods<S>[K] extends (
    request: infer R,
    options?: CallOptions,
  ) => infer Res
    ? (request?: R, options?: CallOptions) => Res
    : never;
};

/** Reports whether a method needs an organization ID. */
export function needsOrganization(method: DescMethod): boolean {
  return (
    !ORGANIZATION_FREE.has(`${method.parent.typeName}.${method.name}`) &&
    method.input.fields.some((f) => f.name === "organization_id")
  );
}

/**
 * Builds an organization-scoped client. An RPC that needs an organization
 * fails before it is sent when neither the request nor the default has one.
 */
export function orgScopedClient<S extends DescService>(
  service: S,
  transport: Transport,
  defaultOrganization: () => string | undefined,
  blocked: Partial<Record<string, string>> = {},
): OrgScopedClient<S> {
  const raw = createClient(service, transport) as unknown as Record<
    string,
    (req: Record<string, unknown>, opts?: CallOptions) => Promise<unknown>
  >;
  const out: Record<string, unknown> = {};
  for (const method of service.methods) {
    if (method.methodKind !== "unary") {
      continue;
    }
    const block = blocked[method.localName];
    if (block !== undefined) {
      out[method.localName] = () =>
        Promise.reject(localError(block, Code.Unimplemented));
      continue;
    }
    const scoped = needsOrganization(method);
    out[method.localName] = (
      request: Record<string, unknown> = {},
      options?: CallOptions,
    ) => {
      if (!scoped) {
        return raw[method.localName]!(request, options);
      }
      const organizationId =
        (request["organizationId"] as string | undefined) ||
        defaultOrganization();
      if (!organizationId) {
        return Promise.reject(
          localError(
            `${method.name} needs an organization: pass organizationId or set it on the client or in OWNSAND_ORG`,
            Code.InvalidArgument,
          ),
        );
      }
      return raw[method.localName]!({ ...request, organizationId }, options);
    };
  }
  return out as OrgScopedClient<S>;
}
