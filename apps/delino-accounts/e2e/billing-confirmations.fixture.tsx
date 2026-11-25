// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import {
  type BillingAccountRole,
  BillingAccountService,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { createRoot } from "react-dom/client";
import { createQueryClient, invalidateServices } from "../src/api.ts";
import { Accounts } from "../src/App.tsx";
import { AuthContextProvider, NOT_CONFIGURED } from "../src/auth.tsx";
import { ACCOUNTS_CONFIG } from "../src/build-config.ts";
import {
  ACCOUNT_ID,
  ADMIN,
  createFakeApi,
  OWNER,
} from "../src/testing/fake-api.ts";

const api = createFakeApi();
const queryClient = createQueryClient();
const accountQueryKey = createConnectQueryKey({
  schema: BillingAccountService.method.getBillingAccount,
  input: { billingAccountId: ACCOUNT_ID },
  cardinality: "finite",
});
queryClient.setQueryDefaults(accountQueryKey, { retry: false });
api.userId = new URLSearchParams(location.search).has("owner") ? OWNER : ADMIN;

const fixture = {
  setRoles(roles: Partial<Record<string, BillingAccountRole>>) {
    for (const member of api.accounts.get(ACCOUNT_ID)!.members) {
      const role = roles[member.userId];
      if (role !== undefined) member.role = role;
    }
    return invalidateServices(queryClient, BillingAccountService);
  },
  failAccountRefresh() {
    api.failNext.set(
      "GetBillingAccount",
      new ConnectError("down", Code.Unavailable),
    );
    return queryClient.refetchQueries({ queryKey: accountQueryKey });
  },
  actionRequests(method: string) {
    return api.calls
      .filter((call) => call.method === method)
      .map(({ request }) => ({
        billingAccountId: String(request.billingAccountId),
        userId: request.userId === undefined ? null : String(request.userId),
      }));
  },
};

declare global {
  interface Window {
    billingConfirmations: typeof fixture;
  }
}

window.billingConfirmations = fixture;
window.history.replaceState(null, "", `/billing/${ACCOUNT_ID}`);
createRoot(document.getElementById("root")!).render(
  <AuthContextProvider
    value={{ ...NOT_CONFIGURED, isAuthenticated: true, userId: api.userId }}
  >
    <Accounts
      config={ACCOUNTS_CONFIG}
      transport={api.transport}
      queryClient={queryClient}
      checkout={undefined}
    />
  </AuthContextProvider>,
);
