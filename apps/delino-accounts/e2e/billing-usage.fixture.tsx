// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { Code, ConnectError } from "@connectrpc/connect";
import { createRoot } from "react-dom/client";
import { createQueryClient } from "../src/api.ts";
import { Accounts } from "../src/App.tsx";
import { AuthContextProvider, NOT_CONFIGURED } from "../src/auth.tsx";
import { ACCOUNTS_CONFIG } from "../src/build-config.ts";
import {
  ACCOUNT_ID,
  addUsageRows,
  createFakeApi,
  OWNER,
} from "../src/testing/fake-api.ts";

const api = createFakeApi();
api.userId = OWNER;
const account = api.accounts.get(ACCOUNT_ID)!;
account.usage.length = 0;
addUsageRows(account, 201, 1_000_000n);

const method = "ListUsageSummary";
const fixture = {
  raiseLastRowCost() {
    account.usage.at(-1)!.amountUsdMicros = 3_000_000n;
  },
  emptyContinuation() {
    account.usage.pop();
    account.usage[0]!.amountUsdMicros = 4_000_000n;
  },
  failNextContinuation() {
    api.failContinuation.set(
      method,
      new ConnectError("busy", Code.ResourceExhausted),
    );
  },
  pageTokens() {
    return api.calls
      .filter((call) => call.method === method)
      .map((call) => call.request.pageToken as string);
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
    value={{ ...NOT_CONFIGURED, isAuthenticated: true, userId: OWNER }}
  >
    <Accounts
      config={ACCOUNTS_CONFIG}
      transport={api.transport}
      queryClient={createQueryClient()}
      checkout={undefined}
    />
  </AuthContextProvider>,
);
