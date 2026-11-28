// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { RemoveMemberRequestSchema } from "@delino/sandrun-protos/v1/member_pb";
import { QueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { Console } from "../src/App.tsx";
import { AuthContextProvider } from "../src/auth.tsx";
import { AuthMode } from "../src/config.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
} from "../src/mock/mock-api.ts";

const mock = createMockTransport();
const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: Infinity, retry: false } },
});
const data = document.body.dataset;
let leaveRequests = 0;
let organizationReads = 0;
let rosterReads = 0;
data.leaveRequests = "0";

const transport: Transport = {
  ...mock,
  async unary(method, signal, timeout, header, input, contextValues) {
    if (method.name === "ListOrganizations") {
      data.organizationReads = String(++organizationReads);
      data.pathAtOrganizationRead = window.location.pathname;
    }
    if (method.name === "ListMembers") {
      data.rosterReads = String(++rosterReads);
    }
    if (method.name === "RemoveMember") {
      const request = input as MessageInitShape<
        typeof RemoveMemberRequestSchema
      >;
      data.leaveRequests = String(++leaveRequests);
      data.leaveOrganizationId = request.organizationId;
      data.leaveUserId = request.userId;
      const result = await new Promise<string>((resolve) => {
        window.addEventListener(
          "sandrun-e2e-leave-result",
          (event) => resolve((event as CustomEvent<string>).detail),
          { once: true },
        );
      });
      if (result === "unavailable") {
        throw new ConnectError("Fixture leave failed", Code.Unavailable);
      }
    }
    return mock.unary(method, signal, timeout, header, input, contextValues);
  },
};

function LeaveOrganizationFixture() {
  const [userId, setUserId] = useState<string>();
  useEffect(() => {
    const setCaller = (event: Event) => {
      const value = (event as CustomEvent<string>).detail;
      setUserId(
        value === "resolved" ? MOCK_USER : value === "" ? "" : undefined,
      );
    };
    window.addEventListener("sandrun-e2e-caller", setCaller);
    return () => window.removeEventListener("sandrun-e2e-caller", setCaller);
  }, []);
  return (
    <AuthContextProvider
      value={{
        isLoading: false,
        isAuthenticated: true,
        userId,
        signIn: async () => {},
        signInFailed: false,
        signOut: async () => {},
        getAccessToken: async () => undefined,
      }}
    >
      <Console
        config={{
          apiOrigin: "https://api.test",
          authMode: AuthMode.Mock,
          logto: undefined,
        }}
        transport={transport}
        queryClient={queryClient}
      />
    </AuthContextProvider>
  );
}

const role = new URLSearchParams(location.search).get("role");
const organizationId =
  role === "owner"
    ? MOCK_IDS.acme
    : role === "admin"
      ? MOCK_IDS.globex
      : MOCK_IDS.northwind;
window.history.replaceState(null, "", `/orgs/${organizationId}/members`);
createRoot(document.getElementById("root")!).render(
  <LeaveOrganizationFixture />,
);
