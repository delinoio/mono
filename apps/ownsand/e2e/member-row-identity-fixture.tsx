// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { Console } from "../src/App.tsx";
import { AuthContextProvider, type Auth } from "../src/auth.tsx";
import { CONSOLE_CONFIG } from "../src/build-config.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
} from "../src/mock/mock-api.ts";

const params = new URLSearchParams(location.search);
const callerId = params.has("owner")
  ? MOCK_USER
  : params.has("member")
    ? "user-member-ada"
    : "user-admin-jun";
const mock = createMockTransport({ userId: callerId });
const mutationLogKey = "ownsand-e2e-member-mutations";
const leaveRequestsKey = "ownsand-e2e-leave-requests";
const leaveRefreshPathKey = "ownsand-e2e-leave-refresh-path";
sessionStorage.setItem(mutationLogKey, "[]");
sessionStorage.setItem(leaveRequestsKey, "[]");
sessionStorage.removeItem(leaveRefreshPathKey);
const memberMutations: string[] = [];
const leaveRequests: { organizationId?: string; userId?: string }[] = [];
const organizationRefresh = new Promise<void>((resolve) => {
  window.addEventListener(
    "ownsand-e2e-release-leave-refresh",
    () => resolve(),
    {
      once: true,
    },
  );
});
const transport: typeof mock = {
  ...mock,
  async unary(method, ...rest) {
    if (
      method.parent.typeName === MemberService.typeName &&
      method.name !== MemberService.method.listMembers.name &&
      method.name !== MemberService.method.listInvitations.name
    ) {
      memberMutations.push(method.name);
      sessionStorage.setItem(mutationLogKey, JSON.stringify(memberMutations));
      if (method.name === MemberService.method.removeMember.name) {
        const { organizationId, userId } =
          rest[3] as (typeof leaveRequests)[number];
        leaveRequests.push({ organizationId, userId });
        sessionStorage.setItem(leaveRequestsKey, JSON.stringify(leaveRequests));
      }
    }
    if (
      method.parent.typeName === OrganizationService.typeName &&
      method.name === OrganizationService.method.listOrganizations.name &&
      leaveRequests.length > 0
    ) {
      sessionStorage.setItem(leaveRefreshPathKey, location.pathname);
      await organizationRefresh;
    }
    return mock.unary(method, ...rest);
  },
};

const signedIn: Auth = {
  isLoading: false,
  isAuthenticated: true,
  userId: undefined,
  signIn: async () => {},
  signInFailed: false,
  signOut: async () => {},
  getAccessToken: async () => undefined,
};

function MemberIdentityFixture() {
  const [userId, setUserId] = useState(
    params.has("owner") ? callerId : params.has("empty") ? "" : undefined,
  );

  useEffect(() => {
    const resolveCaller = () => setUserId(callerId);
    window.addEventListener("ownsand-e2e-resolve-member-caller", resolveCaller);
    return () =>
      window.removeEventListener(
        "ownsand-e2e-resolve-member-caller",
        resolveCaller,
      );
  }, []);

  return (
    <AuthContextProvider value={{ ...signedIn, userId }}>
      <Console config={CONSOLE_CONFIG} transport={transport} />
    </AuthContextProvider>
  );
}

history.replaceState(null, "", `/orgs/${MOCK_IDS.acme}/members`);
const root = document.getElementById("root");
if (!root) throw new Error("Missing #root element");
createRoot(root).render(<MemberIdentityFixture />);
