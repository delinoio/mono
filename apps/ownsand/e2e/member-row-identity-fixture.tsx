// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { Console } from "../src/App.tsx";
import { AuthContextProvider, type Auth } from "../src/auth.tsx";
import { CONSOLE_CONFIG } from "../src/build-config.ts";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

const callerIsMember = new URLSearchParams(location.search).has("member");
const callerId = callerIsMember ? "user-member-ada" : "user-admin-jun";
const mock = createMockTransport({ userId: callerId });
const mutationLogKey = "ownsand-e2e-member-mutations";
sessionStorage.setItem(mutationLogKey, "[]");
const memberMutations: string[] = [];
const transport: typeof mock = {
  ...mock,
  unary(method, ...rest) {
    if (
      method.parent.typeName === MemberService.typeName &&
      method.name !== MemberService.method.listMembers.name &&
      method.name !== MemberService.method.listInvitations.name
    ) {
      memberMutations.push(method.name);
      sessionStorage.setItem(mutationLogKey, JSON.stringify(memberMutations));
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
  const [userId, setUserId] = useState<string>();

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
