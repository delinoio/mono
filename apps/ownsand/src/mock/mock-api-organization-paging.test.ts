// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";

/** The page token that the control plane builds: base64url("v1:" + last ID). */
const tokenOf = (id: string) =>
  btoa(`v1:${id}`).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

function newClient() {
  return createClient(OrganizationService, createMockTransport());
}

async function createOrganizations(
  client: ReturnType<typeof newClient>,
  count: number,
): Promise<string[]> {
  const ids: string[] = [];
  for (let i = 0; i < count; i++) {
    const { organization } = await client.createOrganization({
      name: `Extra ${i}`,
    });
    ids.push(organization!.organizationId);
  }
  return ids;
}

async function listIds(
  client: ReturnType<typeof newClient>,
  request: { pageSize?: number; pageToken?: string } = {},
) {
  const page = await client.listOrganizations(request);
  return {
    ids: page.organizations.map((o) => o.organizationId),
    nextPageToken: page.nextPageToken,
  };
}

const ascending = (ids: readonly string[]) =>
  [...ids].sort((a, b) =>
    a.toLowerCase() < b.toLowerCase()
      ? -1
      : a.toLowerCase() > b.toLowerCase()
        ? 1
        : 0,
  );

describe("mock organization list order and cursor", () => {
  test("lists the caller's organizations in ascending ID order", async () => {
    const client = newClient();
    const created = await createOrganizations(client, 5);
    const { ids } = await listIds(client, { pageSize: 200 });
    // The caller belongs to Acme and Northwind, not to Contoso, and the
    // created organizations have IDs that sort after the seeded ones.
    expect(ids).toEqual([MOCK_IDS.acme, MOCK_IDS.northwind, ...created]);
    expect(ids).toEqual(ascending(ids));
  });

  test("a walk with page size 1 returns the same rows as one large page", async () => {
    const client = newClient();
    await createOrganizations(client, 3);
    const all = (await listIds(client, { pageSize: 200 })).ids;
    const walked: string[] = [];
    let pageToken = "";
    do {
      // A cursor that does not advance must fail the test, not loop forever.
      expect(walked.length).toBeLessThanOrEqual(all.length);
      const page = await listIds(client, { pageSize: 1, pageToken });
      expect(page.ids.length).toBeLessThanOrEqual(1);
      walked.push(...page.ids);
      pageToken = page.nextPageToken;
    } while (pageToken !== "");
    expect(walked).toEqual(all);
  });

  test("a cursor compares by ID and still works after its row is deleted", async () => {
    const client = newClient();
    const [c1, c2, c3] = await createOrganizations(client, 3);
    const first = await listIds(client, { pageSize: 3 });
    expect(first.ids).toEqual([MOCK_IDS.acme, MOCK_IDS.northwind, c1]);
    await client.deleteOrganization({ organizationId: c1! });
    const second = await listIds(client, {
      pageSize: 3,
      pageToken: first.nextPageToken,
    });
    expect(second.ids).toEqual([c2, c3]);
    expect(second.nextPageToken).toBe("");
  });

  test("an organization created between pages sorts after the cursor and is listed once", async () => {
    const client = newClient();
    const [c1] = await createOrganizations(client, 1);
    const first = await listIds(client, { pageSize: 3 });
    expect(first.ids).toEqual([MOCK_IDS.acme, MOCK_IDS.northwind, c1]);
    const [c2, c3] = await createOrganizations(client, 2);
    const second = await listIds(client, {
      pageSize: 3,
      pageToken: first.nextPageToken,
    });
    expect(second.ids).toEqual([c2, c3]);
  });

  test("a token that names an ID the caller does not hold resumes after that ID", async () => {
    const client = newClient();
    const [c1, c2] = await createOrganizations(client, 2);
    // Contoso sorts between Northwind and the created organizations, and the
    // caller is not a member of it.
    const page = await listIds(client, {
      pageToken: tokenOf(MOCK_IDS.contoso.toUpperCase()),
    });
    expect(page.ids).toEqual([c1, c2]);
    expect(page.nextPageToken).toBe("");
  });

  test("rejects a token that is not an encoded UUID v7", async () => {
    const client = newClient();
    for (const pageToken of [MOCK_IDS.acme, tokenOf("not-an-id")]) {
      const error = await client
        .listOrganizations({ pageToken })
        .catch((e: unknown) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect((error as ConnectError).code).toBe(Code.InvalidArgument);
    }
  });
});
