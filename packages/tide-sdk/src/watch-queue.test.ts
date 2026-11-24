// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { RequestState, RunnerState } from "@delino/tide-protos/v1/common_pb";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  WorkService,
  WatchQueueResponseSchema,
} from "@delino/tide-protos/v1/work_pb";
import { MAX_BACKOFF_MS } from "./backoff.ts";
import { TideClient } from "./client.ts";
import { TideConfigError } from "./errors.ts";
import {
  FAKE_ORIGIN,
  fakeServer,
  RecordingClock,
} from "./testing/fake-server.ts";
import {
  watchQueue,
  type WatchQueueCaller,
  type WatchQueueEvent,
  WatchQueueEventKind,
} from "./watch-queue.ts";

const ORG = "0193a000-0000-7000-8000-000000000001";

type Step = "snapshot" | "change" | "keepalive" | "unavailable" | "hang";

/** A WatchQueue server whose stream number `n` runs `scripts[n]`. */
function watchServer(scripts: Step[][], options = {}) {
  let stream = 0;
  return fakeServer((router) => {
    router.service(WorkService, {
      async *watchQueue(
        req,
        ctx,
      ): AsyncGenerator<MessageInitShape<typeof WatchQueueResponseSchema>> {
        expect(req.organizationId).toBe(ORG);
        const script = scripts[stream++] ?? ["hang"];
        for (const step of script) {
          switch (step) {
            case "snapshot":
              yield {
                event: {
                  case: "request",
                  value: { requestId: "q1", state: RequestState.QUEUED },
                },
              };
              yield {
                event: {
                  case: "runner",
                  value: { runnerId: "w1", state: RunnerState.WARM },
                },
              };
              yield { event: { case: "snapshotComplete", value: {} } };
              break;
            case "change":
              yield {
                event: {
                  case: "request",
                  value: { requestId: "q1", state: RequestState.RUNNING },
                },
              };
              break;
            case "keepalive":
              yield { event: { case: "keepalive", value: {} } };
              break;
            case "unavailable":
              throw new ConnectError("restarting", Code.Unavailable);
            case "hang":
              await new Promise((_, reject) =>
                ctx.signal.addEventListener("abort", () =>
                  reject(ctx.signal.reason),
                ),
              );
          }
        }
      },
    });
  }, options);
}

function summarize(event: WatchQueueEvent): string {
  switch (event.kind) {
    case WatchQueueEventKind.Request:
      return `request ${event.request.requestId} ${RequestState[event.request.state]}`;
    case WatchQueueEventKind.Runner:
      return `runner ${event.runner.runnerId} ${RunnerState[event.runner.state]}`;
    case WatchQueueEventKind.SnapshotComplete:
      return "snapshot complete";
    case WatchQueueEventKind.Gap:
      return `gap ${event.failures} ${event.delayMs}`;
  }
}

async function take(
  events: AsyncIterable<WatchQueueEvent>,
  count: number,
  abort: AbortController,
): Promise<string[]> {
  const out: string[] = [];
  for await (const event of events) {
    out.push(summarize(event));
    if (out.length === count) {
      abort.abort();
    }
  }
  return out;
}

describe("watchQueue", () => {
  test("reconnects after each stream end and reports a gap", async () => {
    const server = watchServer([
      ["snapshot", "keepalive", "change"],
      ["unavailable"],
      ["keepalive", "unavailable"],
      ["snapshot"],
    ]);
    const clock = new RecordingClock();
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
      clock,
    });
    const abort = new AbortController();
    const events = await take(
      client.watchQueue({ organizationId: ORG, signal: abort.signal }),
      10,
      abort,
    );
    expect(events).toEqual([
      "request q1 QUEUED",
      "runner w1 WARM",
      "snapshot complete",
      "request q1 RUNNING",
      // A clean end after a complete snapshot waits the first delay.
      "gap 0 1000",
      // Streams that end before their snapshot double the delay.
      "gap 1 1000",
      "gap 2 2000",
      "request q1 QUEUED",
      "runner w1 WARM",
      "snapshot complete",
    ]);
    expect(clock.sleeps).toEqual([1000, 1000, 2000]);
    expect(server.callsOf("WatchQueue").length).toBe(4);
  });

  test("resets the backoff after a complete snapshot and caps it at 30 seconds", async () => {
    const failures: Step[][] = Array.from({ length: 8 }, () => ["unavailable"]);
    const server = watchServer([
      ...failures,
      ["snapshot", "unavailable"],
      ["hang"],
    ]);
    const clock = new RecordingClock();
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
      clock,
    });
    const abort = new AbortController();
    await take(
      client.watchQueue({ organizationId: ORG, signal: abort.signal }),
      8 + 3 + 1,
      abort,
    );
    expect(clock.sleeps).toEqual([
      1000,
      2000,
      4000,
      8000,
      16000,
      MAX_BACKOFF_MS,
      MAX_BACKOFF_MS,
      MAX_BACKOFF_MS,
      1000,
    ]);
  });

  test("reconnects after a network failure", async () => {
    const server = watchServer([["snapshot"], ["snapshot"]], {
      failFetch: (_call: unknown, index: number) => index === 0,
    });
    const clock = new RecordingClock();
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
      clock,
    });
    const abort = new AbortController();
    const events = await take(
      client.watchQueue({ organizationId: ORG, signal: abort.signal }),
      4,
      abort,
    );
    expect(events).toEqual([
      "gap 1 1000",
      "request q1 QUEUED",
      "runner w1 WARM",
      "snapshot complete",
    ]);
  });

  test("redacts an echoed normalized token from a terminal stream status", async () => {
    const server = fakeServer((router) => {
      router.service(WorkService, {
        async *watchQueue(_req, ctx) {
          yield { event: { case: "snapshotComplete", value: {} } };
          const auth = ctx.requestHeader.get("authorization");
          throw new ConnectError(
            `no access with ${auth}`,
            Code.PermissionDenied,
            {
              "x-tide-test": "rejected",
            },
          );
        },
      });
    });
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "secret-access-token\r\n",
      clock: new RecordingClock(),
    });
    const stream = client.watchQueue({ organizationId: ORG });
    expect((await stream.next()).value).toEqual({
      kind: WatchQueueEventKind.SnapshotComplete,
    });
    const error = (await stream
      .next()
      .catch((e: unknown) => e)) as ConnectError;
    expect(server.calls[0]?.headers.get("authorization")).toBe(
      "Bearer secret-access-token",
    );
    expect(error).toBeInstanceOf(ConnectError);
    expect(error.code).toBe(Code.PermissionDenied);
    expect(error.metadata.get("x-tide-test")).toBe("rejected");
    expect(error.message).toBe(
      "[permission_denied] no access with Bearer [redacted]",
    );
    expect(error.rawMessage).toBe("no access with Bearer [redacted]");
  });

  test("redacts the retained cause of a stream body read error", async () => {
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      token: () => "secret-access-token ",
      fetch: async (_input, init) => {
        const auth = new Headers(init?.headers).get("authorization");
        const body = new ReadableStream({
          start(controller) {
            controller.error(new TypeError(`body read failed with ${auth}`));
          },
        });
        return new Response(body, {
          headers: { "content-type": "application/connect+proto" },
        });
      },
    });
    const stream = client.work
      .watchQueue({ organizationId: ORG })
      [Symbol.asyncIterator]();
    const error = (await stream
      .next()
      .catch((e: unknown) => e)) as ConnectError;
    expect(error).toBeInstanceOf(ConnectError);
    expect(error.code).toBe(Code.Unknown);
    const message = "body read failed with Bearer [redacted]";
    expect(error.message).toBe(`[unknown] ${message}`);
    expect(error.rawMessage).toBe(message);
    expect(error.cause).toBeInstanceOf(TypeError);
    expect((error.cause as TypeError).message).toBe(message);
  });

  test("ends with the configuration error of a token source that returns no token", async () => {
    const server = watchServer([["snapshot"]]);
    const clock = new RecordingClock();
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: async () => "",
      clock,
    });
    const error = await (async () => {
      for await (const _ of client.watchQueue({ organizationId: ORG })) {
        // No events.
      }
    })().catch((e: unknown) => e);
    expect((error as ConnectError).cause).toBeInstanceOf(TideConfigError);
    expect(clock.sleeps).toEqual([]);
    expect(server.calls).toEqual([]);
  });

  test("ends without an error when the signal aborts a pending token source", async () => {
    const server = watchServer([["snapshot"]]);
    const abort = new AbortController();
    let asked = 0;
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: () => {
        asked++;
        // Abort while the interceptor waits for the token.
        setTimeout(() => abort.abort(), 0);
        return new Promise<string>(() => {});
      },
      clock: new RecordingClock(),
    });
    const events = await take(
      client.watchQueue({ organizationId: ORG, signal: abort.signal }),
      1,
      abort,
    );
    expect(events).toEqual([]);
    expect(asked).toBe(1);
    expect(server.calls).toEqual([]);
  });

  const firstRequest = create(WatchQueueResponseSchema, {
    event: { case: "request", value: { requestId: "first" } },
  });
  const bufferedRequest = create(WatchQueueResponseSchema, {
    event: { case: "request", value: { requestId: "buffered" } },
  });
  const bufferedSnapshot = create(WatchQueueResponseSchema, {
    event: { case: "snapshotComplete", value: {} },
  });

  test.each([
    ["request", bufferedRequest, "request buffered UNSPECIFIED"],
    ["snapshot completion", bufferedSnapshot, "snapshot complete"],
  ] as const)(
    "closes the stream when a buffered %s arrives after abort",
    async (_name, bufferedResponse, expected) => {
      const clock = new RecordingClock();
      let streams = 0;
      let closed = 0;
      const caller: WatchQueueCaller = {
        async *watchQueue() {
          streams++;
          try {
            yield firstRequest;
            yield bufferedResponse;
          } finally {
            closed++;
          }
        },
      };

      const abort = new AbortController();
      const events = watchQueue(caller, {
        organizationId: ORG,
        signal: abort.signal,
        clock,
      });
      expect(summarize((await events.next()).value!)).toBe(
        "request first UNSPECIFIED",
      );
      abort.abort(new Error("stop the watch"));
      expect(await events.next()).toEqual({ done: true, value: undefined });
      expect(closed).toBe(1);
      expect(streams).toBe(1);
      expect(clock.sleeps).toEqual([]);

      const live = watchQueue(caller, {
        organizationId: ORG,
        signal: new AbortController().signal,
        clock,
      });
      expect(summarize((await live.next()).value!)).toBe(
        "request first UNSPECIFIED",
      );
      expect(summarize((await live.next()).value!)).toBe(expected);
      await live.return();
    },
  );

  test("ends without a gap if the signal aborts as the backoff ends", async () => {
    const abort = new AbortController();
    let streams = 0;
    const events = watchQueue(
      {
        async *watchQueue() {
          streams++;
        },
      },
      {
        organizationId: ORG,
        signal: abort.signal,
        clock: {
          now: () => 0,
          sleep: () => {
            const wait = Promise.withResolvers<void>();
            wait.resolve();
            abort.abort();
            return wait.promise;
          },
        },
      },
    );
    expect(await events.next()).toEqual({ done: true, value: undefined });
    expect(streams).toBe(1);
  });

  test("ends without reconnecting if the signal aborts after a gap", async () => {
    const abort = new AbortController();
    let streams = 0;
    const events = watchQueue(
      {
        async *watchQueue() {
          streams++;
        },
      },
      {
        organizationId: ORG,
        signal: abort.signal,
        clock: new RecordingClock(),
      },
    );
    expect(summarize((await events.next()).value!)).toBe("gap 1 1000");
    abort.abort();
    expect(await events.next()).toEqual({ done: true, value: undefined });
    expect(streams).toBe(1);
  });

  test("ends without an error when the signal aborts an open stream", async () => {
    const server = watchServer([["snapshot", "hang"]]);
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
      clock: new RecordingClock(),
    });
    const abort = new AbortController();
    const events = await take(
      client.watchQueue({ organizationId: ORG, signal: abort.signal }),
      3,
      abort,
    );
    expect(events.at(-1)).toBe("snapshot complete");
  });
});
