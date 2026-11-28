// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { Code } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Console } from "../src/App.tsx";
import { createQueryClient } from "../src/api.ts";
import { MockAuthProvider } from "../src/auth.tsx";
import { API_RESOURCE, AuthMode } from "../src/config.ts";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";
import { runnerGroupFixture } from "./runner-group-transport.ts";

const queryClient = createQueryClient();
const fixture = runnerGroupFixture(createMockTransport());
const queryKey = createConnectQueryKey({
  schema: GitHubService.method.listGitHubRunnerGroups,
  cardinality: undefined,
});

function holdRefresh() {
  const release = fixture.holdNextList();
  const refreshed = queryClient.refetchQueries({ queryKey, type: "active" });
  return async (result: "default-only" | "failure") => {
    if (result === "default-only") fixture.setGroups([1n]);
    else fixture.fail(Code.Internal);
    release();
    await refreshed;
    fixture.fail();
  };
}

declare global {
  interface Window {
    sandrunRunnerGroupFixture: {
      holdRefresh: typeof holdRefresh;
      releaseRefresh?: ReturnType<typeof holdRefresh>;
      updates: typeof fixture.updates;
    };
  }
}
window.sandrunRunnerGroupFixture = { holdRefresh, updates: fixture.updates };

window.history.replaceState(null, "", `/orgs/${MOCK_IDS.acme}/integrations`);
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <MockAuthProvider>
      <Console
        config={{
          apiOrigin: API_RESOURCE,
          authMode: AuthMode.Mock,
          logto: undefined,
        }}
        queryClient={queryClient}
        transport={fixture.transport}
      />
    </MockAuthProvider>
  </StrictMode>,
);
