// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";
import { walkFixturePages } from "./walk-fixture-pages.ts";

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
    const created = await createOrganizations(client, 3);
    const all = (await listIds(client, { pageSize: 200 })).ids;
    const walked = await walkFixturePages(
      2 + created.length,
      1,
      async (request) => {
        const page = await listIds(client, request);
        expect(page.ids.length).toBeLessThanOrEqual(1);
        return { rows: page.ids, nextPageToken: page.nextPageToken };
      },
    );
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

describe("pagination fixture guard", () => {
  function stubPages(pages: { rows: string[]; nextPageToken: string }[]) {
    const requests: string[] = [];
    return {
      requests,
      async listPage({ pageToken }: { pageToken: string }) {
        const page = pages[requests.length];
        requests.push(pageToken);
        if (!page) {
          throw new Error("Unexpected page request");
        }
        return page;
      },
    };
  }

  test("rejects an empty page that repeats the request token", async () => {
    const { requests, listPage } = stubPages([
      { rows: ["id-1"], nextPageToken: "cursor-a" },
      { rows: [], nextPageToken: "cursor-a" },
    ]);

    await expect(walkFixturePages(2, 1, listPage)).rejects.toThrow(
      'Pagination repeated continuation token "cursor-a"',
    );
    expect(requests).toEqual(["", "cursor-a"]);
  });

  test("rejects a token that repeats after an intervening token", async () => {
    const { requests, listPage } = stubPages([
      { rows: ["id-1"], nextPageToken: "cursor-a" },
      { rows: ["id-2"], nextPageToken: "cursor-b" },
      { rows: [], nextPageToken: "cursor-a" },
    ]);

    await expect(walkFixturePages(2, 1, listPage)).rejects.toThrow(
      'Pagination repeated continuation token "cursor-a"',
    );
    expect(requests).toEqual(["", "cursor-a", "cursor-b"]);
  });

  test("rejects a distinct token on the last allowed response", async () => {
    const { requests, listPage } = stubPages([
      { rows: ["id-1"], nextPageToken: "cursor-a" },
      { rows: ["id-2"], nextPageToken: "cursor-b" },
      { rows: [], nextPageToken: "cursor-c" },
    ]);

    await expect(walkFixturePages(2, 1, listPage)).rejects.toThrow(
      "Pagination reached the request limit of 3 with a continuation token",
    );
    expect(requests).toEqual(["", "cursor-a", "cursor-b"]);
  });

  test("accepts an empty terminal page after five full pages", async () => {
    const ids = ["id-1", "id-2", "id-3", "id-4", "id-5"];
    const cursors = [
      "cursor-a",
      "cursor-b",
      "cursor-c",
      "cursor-d",
      "cursor-e",
    ];
    const { requests, listPage } = stubPages([
      ...ids.map((id, index) => ({
        rows: [id],
        nextPageToken: cursors[index]!,
      })),
      { rows: [], nextPageToken: "" },
    ]);

    await expect(walkFixturePages(5, 1, listPage)).resolves.toEqual(ids);
    expect(requests).toEqual(["", ...cursors]);
  });
});
