// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { Role } from "@delino/sandrun-protos/v1/common_pb";
import { MemberService } from "@delino/sandrun-protos/v1/member_pb";
import { createRoot } from "react-dom/client";
import { App } from "../src/App.tsx";
import { API_RESOURCE, AuthMode, REQUIRED_API_SCOPE } from "../src/config.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
} from "../src/mock/mock-api.ts";

const endpoint = "https://logto.test";
const appId = "direct-action-fixture";
const params = new URLSearchParams(window.location.search);
const action = params.get("action");
const failure = params.get("failure");
const delay = params.get("delay") === "true";

async function start() {
  const mock = createMockTransport({ failFirstAcmeMemberContinuation: false });
  if (action === "RevokeInvitation") {
    await createClient(MemberService, mock).createInvitation({
      organizationId: MOCK_IDS.acme,
      role: Role.MEMBER,
    });
  }

  let actionRequests = 0;
  const transport: Transport = {
    ...mock,
    async unary(method, ...args) {
      if (method.name !== action) return mock.unary(method, ...args);
      document.body.dataset.actionRequests = String(++actionRequests);
      if (delay) {
        if (actionRequests > 1) return mock.unary(method, ...args);
        await new Promise<void>((resolve) => {
          window.addEventListener("release-direct-action", () => resolve(), {
            once: true,
          });
        });
      }
      document.body.dataset.actionRejected = "true";
      throw new ConnectError(
        "Fixture direct action failure",
        failure === "permission" ? Code.PermissionDenied : Code.Unauthenticated,
      );
    },
  };

  // Restore a synthetic session so the real Logto provider handles sign-in.
  const now = Math.floor(Date.now() / 1000);
  const expiration = now + 3600;
  const claims = {
    iss: `${endpoint}/oidc`,
    aud: appId,
    sub: MOCK_USER,
    iat: now,
    exp: expiration,
  };
  localStorage.setItem(
    `logto:${appId}:idToken`,
    `header.${btoa(JSON.stringify(claims)).replace(/=+$/, "")}.signature`,
  );
  localStorage.setItem(
    `logto:${appId}:accessToken`,
    JSON.stringify({
      [`@${API_RESOURCE}`]: {
        token: "fixture-access-token",
        scope: REQUIRED_API_SCOPE,
        expiresAt: expiration,
      },
    }),
  );
  const page = action === "CreateGitHubInstallUrl" ? "integrations" : "members";
  window.history.replaceState(null, "", `/orgs/${MOCK_IDS.acme}/${page}`);
  createRoot(document.getElementById("root")!).render(
    <App
      config={{
        apiOrigin: API_RESOURCE,
        authMode: AuthMode.Logto,
        logto: { endpoint, appId, scopes: [REQUIRED_API_SCOPE] },
      }}
      mockTransport={transport}
    />,
  );
}

void start();
