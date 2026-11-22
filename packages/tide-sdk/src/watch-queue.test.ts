// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { RequestState, RunnerState } from "@delino/tide-protos/v1/common_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  WorkService,
  type WatchQueueResponseSchema,
} from "@delino/tide-protos/v1/work_pb";
import { MAX_BACKOFF_MS } from "./backoff.ts";
import { TideClient } from "./client.ts";
import { TideConfigError } from "./errors.ts";
import {
  FAKE_ORIGIN,
  fakeServer,
  RecordingClock,
} from "./testing/fake-server.ts";
import { type WatchQueueEvent, WatchQueueEventKind } from "./watch-queue.ts";

const ORG = "0193a000-0000-7000-8000-000000000001";

type Step =
  "snapshot" | "change" | "keepalive" | "unavailable" | "denied" | "hang";

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
            case "denied":
              throw new ConnectError("no access", Code.PermissionDenied);
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

  test("ends with the server's error for a status that is not retryable", async () => {
    const server = watchServer([["denied"]]);
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
      clock: new RecordingClock(),
    });
    const error = await (async () => {
      for await (const _ of client.watchQueue({ organizationId: ORG })) {
        // No events.
      }
    })().catch((e: unknown) => e);
    expect((error as ConnectError).code).toBe(Code.PermissionDenied);
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
