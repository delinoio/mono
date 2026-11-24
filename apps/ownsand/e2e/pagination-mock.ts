// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
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

export function createMockTransport(): Transport {
  const methodName = sessionStorage.getItem("ownsand-e2e-paged-list");
  const pagination =
    methodName !== null && Object.hasOwn(lists, methodName)
      ? lists[methodName as keyof typeof lists]
      : undefined;
  // Seed enough rows to split the selected list across two pages.
  const base = createBaseMock({
    extraApiKeys: Number(methodName === "ListApiKeys"),
    extraEnrollmentTokens: Number(methodName === "ListEnrollmentTokens"),
  });
  return {
    ...base,
    async unary(method, signal, timeout, header, input, contextValues) {
      if (method.name === "ListOrganizations") {
        await failOrganizationListIfConfigured();
      }
      if (method.name !== methodName || pagination === undefined) {
        return base.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
      }
      const { field, firstPageSize } = pagination;
      const request = input as typeof input & { pageToken?: string };
      const token = request.pageToken ?? "";
      const tokens = JSON.parse(
        sessionStorage.getItem("ownsand-e2e-page-tokens") ?? "[]",
      ) as string[];
      sessionStorage.setItem(
        "ownsand-e2e-page-tokens",
        JSON.stringify([...tokens, token]),
      );
      if (token !== "") {
        if (token !== CONTINUATION) {
          throw new ConnectError(
            "Unexpected continuation token",
            Code.InvalidArgument,
          );
        }
        if (sessionStorage.getItem("ownsand-e2e-page-error") === "hold") {
          await new Promise<void>((resolve) => {
            window.addEventListener(
              "ownsand-e2e-release-page",
              () => resolve(),
              { once: true },
            );
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
