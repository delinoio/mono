// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import type { ListUsageSummaryRequest } from "@delino/accounts-protos/v1/usage_pb";
import { createRoot } from "react-dom/client";
import { createQueryClient } from "../src/api.ts";
import { Accounts } from "../src/App.tsx";
import { AuthContextProvider, NOT_CONFIGURED } from "../src/auth.tsx";
import { ACCOUNTS_CONFIG } from "../src/build-config.ts";
import { ACCOUNT_ID, createFakeApi, VIEWER } from "../src/testing/fake-api.ts";

const api = createFakeApi();
api.userId = VIEWER;

const fixture = {
  summaryMonths() {
    return api.calls
      .filter(({ method }) => method === "ListUsageSummary")
      .map(({ request }) => {
        const { year, month } = request as ListUsageSummaryRequest;
        return [year, month];
      });
  },
};

declare global {
  interface Window {
    billingUsageFixture: typeof fixture;
  }
}

window.billingUsageFixture = fixture;
window.history.replaceState(null, "", `/billing/${ACCOUNT_ID}`);
createRoot(document.getElementById("root")!).render(
  <AuthContextProvider
    value={{ ...NOT_CONFIGURED, isAuthenticated: true, userId: VIEWER }}
  >
    <Accounts
      config={ACCOUNTS_CONFIG}
      transport={api.transport}
      queryClient={createQueryClient()}
      checkout={undefined}
    />
  </AuthContextProvider>,
);
