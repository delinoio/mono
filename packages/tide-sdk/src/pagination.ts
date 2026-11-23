// SPDX-License-Identifier: Apache-2.0
import type { DescMessage, MessageInitShape } from "@bufbuild/protobuf";
import type { CallOptions } from "@connectrpc/connect";
import { TideError } from "./errors.ts";

/** A list request without its page token, which the iterator sets. */
export type PageRequest<Schema extends DescMessage> = Omit<
  MessageInitShape<Schema>,
  "pageToken" | "$typeName"
>;

/**
 * Iterates over every item of a list RPC. It calls the RPC with the request
 * and then with each `next_page_token` until the server returns an empty
 * token. The request's `page_size` applies to every page. Errors of a page
 * end the iteration with the `ConnectError` of the call. A server that
 * returns the same page token twice ends the iteration with a `TideError`
 * instead of looping forever.
 */
export async function* paginate<
  Request extends object,
  Response extends { nextPageToken: string },
  Item,
>(
  call: (
    request: Request & { pageToken: string },
    options?: CallOptions,
  ) => Promise<Response>,
  request: Request,
  items: (response: Response) => readonly Item[],
  options?: CallOptions,
): AsyncGenerator<Item, void, undefined> {
  const seen = new Set<string>();
  let pageToken = "";
  for (;;) {
    const response = await call({ ...request, pageToken }, options);
    yield* items(response);
    if (response.nextPageToken === "") {
      return;
    }
    if (seen.has(response.nextPageToken)) {
      throw new TideError("the server repeated a page token");
    }
    seen.add(response.nextPageToken);
    pageToken = response.nextPageToken;
  }
}

/** Collects every item of an async iterable into an array. */
export async function collect<T>(items: AsyncIterable<T>): Promise<T[]> {
  const out: T[] = [];
  for await (const item of items) {
    out.push(item);
  }
  return out;
}
