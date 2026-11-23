// SPDX-License-Identifier: Apache-2.0
import { type ConnectRouter, createConnectRouter } from "@connectrpc/connect";
import { createFetchHandler } from "@connectrpc/connect/protocol";
import type { Clock } from "../clock.ts";

/** The API origin of the fake server. */
export const FAKE_ORIGIN = "https://tide.test";

export interface RecordedCall {
  url: string;
  /** `/<service>/<method>`. */
  path: string;
  headers: Headers;
}

export interface FakeServer {
  /** A fetch that serves the routes in memory with the Connect protocol. */
  fetch: typeof globalThis.fetch;
  calls: RecordedCall[];
  /** The recorded calls of one method name, for example `ListRequests`. */
  callsOf(method: string): RecordedCall[];
}

/**
 * A fake tide control plane: a Connect router with the test's service
 * implementations behind a fetch function, so the SDK runs its real
 * Connect-Web transport, headers and error decoding against it.
 * `failFetch` makes the fetch of the given call reject, like a network
 * failure, before the server sees the call.
 */
export function fakeServer(
  routes: (router: ConnectRouter) => void,
  options: { failFetch?: (call: RecordedCall, index: number) => boolean } = {},
): FakeServer {
  const router = createConnectRouter();
  routes(router);
  const handlers = new Map(
    router.handlers.map((handler) => [
      handler.requestPath,
      createFetchHandler(handler),
    ]),
  );
  const calls: RecordedCall[] = [];
  const fetch: typeof globalThis.fetch = async (input, init) => {
    const request = new Request(input, init);
    const url = new URL(request.url);
    const call = {
      url: request.url,
      path: url.pathname,
      headers: new Headers(request.headers),
    };
    calls.push(call);
    if (options.failFetch?.(call, calls.length - 1)) {
      throw new TypeError("fetch failed");
    }
    const handler = handlers.get(url.pathname);
    if (handler === undefined) {
      return new Response(null, { status: 404 });
    }
    return handler(request);
  };
  return {
    fetch,
    calls,
    callsOf: (method) => calls.filter((c) => c.path.endsWith(`/${method}`)),
  };
}

/**
 * A clock whose sleeps return at once and are recorded, so reconnect tests
 * check the backoff delays without waiting.
 */
export class RecordingClock implements Clock {
  readonly sleeps: number[] = [];
  #now = Date.UTC(2026, 0, 1);

  now(): number {
    return this.#now;
  }

  async sleep(ms: number, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted();
    this.sleeps.push(ms);
    this.#now += ms;
  }
}
