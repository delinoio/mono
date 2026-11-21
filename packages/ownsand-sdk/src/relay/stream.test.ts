// SPDX-License-Identifier: Apache-2.0
import { getEventListeners } from "node:events";
import { describe, expect, test } from "@rstest/core";
import { ActivityRegistry } from "../connection.ts";
import { VirtualClock } from "../testing/virtual-clock.ts";
import type { OperationContext } from "./context.ts";
import {
  AttemptDeadlineError,
  relayCallWithDeadline,
  watchedRelayStream,
} from "./stream.ts";

function setup() {
  const clock = new VirtualClock();
  const ctx = {
    clock,
    activity: new ActivityRegistry(),
  } as unknown as OperationContext;
  return { clock, ctx };
}

const count = (signal: AbortSignal) =>
  getEventListeners(signal, "abort").length;

describe("relayCallWithDeadline caller signal listeners", () => {
  test("leaves no listener after 12 sequential calls", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    for (let i = 0; i < 12; i++) {
      await relayCallWithDeadline(ctx, "g", caller.signal, 1000, async () => i);
      expect(count(caller.signal)).toBe(0);
    }
  });

  test("leaves no listener after a rejected call", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    await expect(
      relayCallWithDeadline(ctx, "g", caller.signal, 1000, async () => {
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");
    expect(count(caller.signal)).toBe(0);
  });

  test("leaves no listener after the deadline", async () => {
    const { ctx, clock } = setup();
    const caller = new AbortController();
    const result = relayCallWithDeadline(
      ctx,
      "g",
      caller.signal,
      1000,
      (options) =>
        new Promise((_, reject) => {
          options.signal.addEventListener("abort", () =>
            reject(options.signal.reason),
          );
        }),
    ).catch((error) => error);
    await clock.advance(1000);
    expect(await result).toBeInstanceOf(AttemptDeadlineError);
    expect(count(caller.signal)).toBe(0);
  });

  test("a caller abort during the call aborts it with the caller reason", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    const reason = new Error("caller stop");
    const result = relayCallWithDeadline(
      ctx,
      "g",
      caller.signal,
      1000,
      (options) =>
        new Promise((_, reject) => {
          options.signal.addEventListener("abort", () =>
            reject(options.signal.reason),
          );
        }),
    ).catch((error) => error);
    caller.abort(reason);
    expect(await result).toBe(reason);
    expect(count(caller.signal)).toBe(0);
  });

  test("an already aborted signal aborts the call at once", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    const reason = new Error("early");
    caller.abort(reason);
    await expect(
      relayCallWithDeadline(ctx, "g", caller.signal, 1000, async (options) => {
        throw options.signal.reason;
      }),
    ).rejects.toBe(reason);
    expect(count(caller.signal)).toBe(0);
  });

  test("a later caller abort after the call ended does not throw", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    await relayCallWithDeadline(ctx, "g", caller.signal, 1000, async () => 1);
    expect(() => caller.abort(new Error("late"))).not.toThrow();
  });
});

describe("watchedRelayStream caller signal listeners", () => {
  async function* items(n: number) {
    for (let i = 0; i < n; i++) yield i;
  }

  test("leaves no listener when the stream completes", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    const seen: number[] = [];
    for (let i = 0; i < 12; i++) {
      for await (const v of watchedRelayStream(ctx, "g", caller.signal, () =>
        items(2),
      )) {
        seen.push(v);
      }
      expect(count(caller.signal)).toBe(0);
    }
    expect(seen.length).toBe(24);
  });

  test("leaves no listener when the stream throws", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    async function* failing() {
      yield 1;
      throw new Error("stream boom");
    }
    await expect(
      (async () => {
        for await (const _ of watchedRelayStream(ctx, "g", caller.signal, () =>
          failing(),
        )) {
          // drain
        }
      })(),
    ).rejects.toThrow("stream boom");
    expect(count(caller.signal)).toBe(0);
  });

  test("leaves no listener after an early return", async () => {
    const { ctx } = setup();
    const caller = new AbortController();
    const stream = watchedRelayStream(ctx, "g", caller.signal, () => items(5));
    await stream.next();
    expect(count(caller.signal)).toBe(1);
    await stream.return(undefined);
    expect(count(caller.signal)).toBe(0);
  });
});
