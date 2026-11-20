// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";

import { markInterruption } from "../errors.ts";
import type { RelayCall, RelayContext } from "./context.ts";
import type { OperationGrant } from "./grant.ts";
import { linkedController, STREAM_WATCHDOG_MS } from "./recovery.ts";

/**
 * Runs one relay server stream under its grant. A watchdog cancels the stream
 * when no response byte (not even of a relay keepalive or of a large message
 * still in transit) arrives for 90 seconds; the stream then fails as a
 * transport interruption. The watchdog does not run while the caller holds a
 * delivered message, because the SDK reads no bytes in that time.
 */
export async function* watchedStream<T>(
  ctx: RelayContext,
  grant: OperationGrant,
  parent: AbortSignal,
  open: (call: RelayCall) => AsyncIterable<T>,
): AsyncGenerator<T, void, undefined> {
  const { controller, dispose } = linkedController(parent);
  const watchdogStop = new AbortController();
  let lastByte = ctx.clock.now();
  let suspended = false;
  let timedOut = false;
  const watchdog = (async () => {
    while (!watchdogStop.signal.aborted) {
      const wait = suspended
        ? STREAM_WATCHDOG_MS
        : lastByte + STREAM_WATCHDOG_MS - ctx.clock.now();
      if (!suspended && wait <= 0) {
        timedOut = true;
        controller.abort(
          new ConnectError(
            "relay stream received no bytes for 90 seconds",
            Code.DeadlineExceeded,
          ),
        );
        return;
      }
      try {
        await ctx.clock.sleep(wait, watchdogStop.signal);
      } catch {
        return;
      }
    }
  })();
  grant.streamOpened();
  try {
    const stream = open({
      signal: controller.signal,
      onBytes: () => (lastByte = ctx.clock.now()),
    });
    for await (const message of stream) {
      lastByte = ctx.clock.now();
      suspended = true;
      try {
        yield message;
      } finally {
        suspended = false;
        lastByte = ctx.clock.now();
      }
    }
  } catch (err) {
    if (timedOut && !parent.aborted) {
      throw markInterruption(
        new ConnectError(
          "relay stream received no bytes for 90 seconds",
          Code.DeadlineExceeded,
        ),
      );
    }
    throw err;
  } finally {
    grant.streamClosed();
    watchdogStop.abort();
    controller.abort();
    dispose();
    await watchdog;
  }
}
