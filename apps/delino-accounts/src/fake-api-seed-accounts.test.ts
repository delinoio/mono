// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { createClient } from "@connectrpc/connect";
import {
  BillingAccountRole,
  BillingAccountService,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  addBillingAccounts,
  createFakeApi,
  OUTSIDER,
  OWNER,
  VIEWER,
} from "./testing/fake-api.ts";

const SEEDED_NAME = "Account 0001";

describe("addBillingAccounts", () => {
  test("seeds the signed-in user as the only owner of each account", async () => {
    const api = createFakeApi();
    api.userId = VIEWER;
    addBillingAccounts(api, 2);
    const client = createClient(BillingAccountService, api.transport);

    const { billingAccounts } = await client.listBillingAccounts({});
    const names = billingAccounts.map((a) => a.name);
    expect(names).toContain(SEEDED_NAME);
    expect(names).toContain("Account 0002");

    const seeded = [...api.accounts.values()].filter((a) =>
      a.name.startsWith("Account "),
    );
    expect(seeded).toHaveLength(2);
    for (const account of seeded) {
      expect(account.members).toHaveLength(1);
      expect(account.members[0]).toMatchObject({
        billingAccountId: account.id,
        userId: VIEWER,
        role: BillingAccountRole.OWNER,
      });
      expect(account.invitations).toEqual([]);
      expect(account.linked).toEqual([]);
    }
  });

  test("excludes the seeded accounts for another user", async () => {
    const api = createFakeApi();
    api.userId = VIEWER;
    addBillingAccounts(api, 1);
    api.userId = OUTSIDER;
    const client = createClient(BillingAccountService, api.transport);

    const { billingAccounts } = await client.listBillingAccounts({});
    expect(billingAccounts.map((a) => a.name)).not.toContain(SEEDED_NAME);
  });

  test("still lists the seeded accounts for the default owner identity", async () => {
    const api = createFakeApi();
    expect(api.userId).toBe(OWNER);
    addBillingAccounts(api, 1);
    const client = createClient(BillingAccountService, api.transport);

    const { billingAccounts } = await client.listBillingAccounts({});
    expect(billingAccounts.map((a) => a.name)).toContain(SEEDED_NAME);
  });
});
