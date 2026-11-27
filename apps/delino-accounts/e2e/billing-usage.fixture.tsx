// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { Code, ConnectError } from "@connectrpc/connect";
import type { ListUsageSummaryRequest } from "@delino/accounts-protos/v1/usage_pb";
import { createRoot } from "react-dom/client";
import { createQueryClient } from "../src/api.ts";
import { Accounts } from "../src/App.tsx";
import { AuthContextProvider, NOT_CONFIGURED } from "../src/auth.tsx";
import { ACCOUNTS_CONFIG } from "../src/build-config.ts";
import {
  ACCOUNT_ID,
  addUsageRows,
  createFakeApi,
  VIEWER,
} from "../src/testing/fake-api.ts";

const api = createFakeApi();
api.userId = VIEWER;

const usageSummaryMethod = "ListUsageSummary";
const account = api.accounts.get(ACCOUNT_ID)!;
const fixture = {
  seedPagination() {
    account.usage.length = 0;
    addUsageRows(account, 201, 1_000_000n);
  },
  summaryMonths() {
    return api.calls
      .filter(({ method }) => method === usageSummaryMethod)
      .map(({ request }) => {
        const { year, month } = request as ListUsageSummaryRequest;
        return [year, month];
      });
  },
  raiseLastRowCost() {
    account.usage.at(-1)!.amountUsdMicros = 3_000_000n;
  },
  emptyContinuation() {
    account.usage.pop();
    account.usage[0]!.amountUsdMicros = 4_000_000n;
  },
  failNextContinuation() {
    api.failContinuation.set(
      usageSummaryMethod,
      new ConnectError("busy", Code.ResourceExhausted),
    );
  },
  pageTokens() {
    return api.calls
      .filter(({ method }) => method === usageSummaryMethod)
      .map(({ request }) => (request as ListUsageSummaryRequest).pageToken);
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
