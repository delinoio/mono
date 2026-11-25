// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "../src/index.css";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  CreditService,
  type UpdateAutoTopUpSettingsRequest,
} from "@delino/accounts-protos/v1/credit_pb";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Accounts } from "../src/App.tsx";
import { createQueryClient, invalidateServices } from "../src/api.ts";
import { AuthContextProvider } from "../src/auth.tsx";
import { ACCOUNT_ID, ADMIN, createFakeApi } from "../src/testing/fake-api.ts";
import { deferNextCall } from "../src/testing/form-submit-deferral.ts";

const api = createFakeApi();
api.userId = ADMIN;
const account = api.accounts.get(ACCOUNT_ID)!;
account.autoTopUp = {
  ...account.autoTopUp,
  enabled: true,
  thresholdUsdMicros: 10_000_000n,
  amountUsdMicros: 50_000_000n,
  monthlyCapUsdMicros: 200_000_000n,
};
const queryClient = createQueryClient();
const updateMethod = "UpdateAutoTopUpSettings";
const readMethod = "GetAutoTopUpSettings";
let update: ReturnType<typeof deferNextCall> | undefined;
let confirmation: ReturnType<typeof deferNextCall> | undefined;

const fixture = {
  holdSave(holdConfirmation = false) {
    update = deferNextCall(api, updateMethod);
    if (holdConfirmation) confirmation = deferNextCall(api, readMethod);
    return this.readCount();
  },
  releaseSave() {
    update?.release();
    return null;
  },
  rejectSave() {
    update?.fail(new ConnectError("Unavailable", Code.Unavailable));
    return null;
  },
  releaseConfirmation() {
    confirmation?.release();
    return null;
  },
  readCount() {
    return api.calls.filter(({ method }) => method === readMethod).length;
  },
  saves() {
    return api.calls
      .filter(({ method }) => method === updateMethod)
      .map(({ request }) => {
        const { settings } = request as UpdateAutoTopUpSettingsRequest;
        if (settings === undefined) return null;
        return {
          enabled: settings.enabled,
          threshold: settings.thresholdUsdMicros.toString(),
          amount: settings.amountUsdMicros.toString(),
          cap: settings.monthlyCapUsdMicros.toString(),
        };
      });
  },
  async refreshCounters() {
    account.autoTopUp = {
      ...account.autoTopUp,
      monthToDateUsdMicros: 30_000_000n,
      consecutiveFailures: 1,
    };
    await invalidateServices(queryClient, CreditService);
    return null;
  },
  async refreshAmount() {
    account.autoTopUp = { ...account.autoTopUp, amountUsdMicros: 40_000_000n };
    await invalidateServices(queryClient, CreditService);
    return null;
  },
};

declare global {
  interface Window {
    autoTopUpFixture: typeof fixture;
  }
}

window.autoTopUpFixture = fixture;
window.history.replaceState(null, "", `/billing/${ACCOUNT_ID}`);
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <AuthContextProvider
      value={{
        isLoading: false,
        isAuthenticated: true,
        userId: ADMIN,
        signIn: async () => {},
        signInFailed: false,
        signOut: async () => {},
        getAccessToken: async () => undefined,
      }}
    >
      <Accounts
        config={{
          apiOrigin: window.location.origin,
          logto: undefined,
          paddle: undefined,
        }}
        transport={api.transport}
        queryClient={queryClient}
        checkout={undefined}
      />
    </AuthContextProvider>
  </StrictMode>,
);
