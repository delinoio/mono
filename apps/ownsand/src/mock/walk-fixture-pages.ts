// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";

export async function walkFixturePages<Row>(
  expectedRows: number,
  pageSize: number,
  listPage: (request: { pageSize: number; pageToken: string }) => Promise<{
    rows: readonly Row[];
    nextPageToken: string;
  }>,
): Promise<Row[]> {
  // A full page can have a continuation token even when no rows remain.
  const requestLimit = Math.floor(expectedRows / pageSize) + 1;
  const seenTokens = new Set<string>();
  const rows: Row[] = [];
  let pageToken = "";

  for (let request = 0; request < requestLimit; request++) {
    assert.ok(
      rows.length <= expectedRows,
      "Pagination exceeded the expected row count",
    );
    const page = await listPage({ pageSize, pageToken });
    rows.push(...page.rows);
    pageToken = page.nextPageToken;
    if (pageToken === "") {
      return rows;
    }
    assert.ok(
      !seenTokens.has(pageToken),
      `Pagination repeated continuation token "${pageToken}"`,
    );
    seenTokens.add(pageToken);
  }

  assert.fail(
    `Pagination reached the request limit of ${requestLimit} with a continuation token`,
  );
}
