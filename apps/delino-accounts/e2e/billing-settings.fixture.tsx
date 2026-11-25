// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { CreditService } from "@delino/accounts-protos/v1/credit_pb";
import { createRoot } from "react-dom/client";
import { createQueryClient, invalidateServices } from "../src/api.ts";
import { Accounts } from "../src/App.tsx";
import { AuthContextProvider, NOT_CONFIGURED } from "../src/auth.tsx";
import { ACCOUNTS_CONFIG } from "../src/build-config.ts";
import { ACCOUNT_ID, ADMIN, createFakeApi } from "../src/testing/fake-api.ts";

const api = createFakeApi();
const queryClient = createQueryClient();
api.userId = ADMIN;
const account = api.accounts.get(ACCOUNT_ID)!;
account.autoTopUp = {
  ...account.autoTopUp,
  monthToDateUsdMicros: 25_000_000n,
};
window.addEventListener("billing-fixture:refresh-cap-usage", () => {
  account.autoTopUp = {
    ...account.autoTopUp,
    monthToDateUsdMicros: 30_000_000n,
    consecutiveFailures: 1,
  };
  void invalidateServices(queryClient, CreditService);
});

window.history.replaceState(null, "", `/billing/${ACCOUNT_ID}`);
createRoot(document.getElementById("root")!).render(
  <AuthContextProvider
    value={{ ...NOT_CONFIGURED, isAuthenticated: true, userId: ADMIN }}
  >
    <Accounts
      config={ACCOUNTS_CONFIG}
      transport={api.transport}
      queryClient={queryClient}
      checkout={undefined}
    />
  </AuthContextProvider>,
);
