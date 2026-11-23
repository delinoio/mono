// SPDX-License-Identifier: Apache-2.0
import type { ConnectError } from "@connectrpc/connect";
import type { FakeApi } from "./fake-api.ts";

/**
 * Holds one call of a fake API method until the test releases it, so a test
 * can close and reopen a dialog while its request is still in flight. Unlike
 * `FakeApi.gates`, a deferral holds only the next call of the method; later
 * calls run at once.
 */
export interface FormSubmitDeferral {
  /** Lets the held call run and succeed. */
  release(): void;
  /** Fails the held call with `error`. */
  fail(error: ConnectError): void;
}

const deferrals = new WeakMap<FakeApi, Map<string, Promise<void>[]>>();

/** Holds the next call of `method` of `api`; deferrals queue in call order. */
export function deferNextCall(
  api: FakeApi,
  method: string,
): FormSubmitDeferral {
  let release!: () => void;
  let fail!: (error: ConnectError) => void;
  const promise = new Promise<void>((resolve, reject) => {
    release = resolve;
    fail = reject;
  });
  // The handler awaits the promise when a call takes it; keep a rejection
  // that arrives before then from being reported as unhandled.
  promise.catch(() => undefined);
  const queues = deferrals.get(api) ?? new Map<string, Promise<void>[]>();
  deferrals.set(api, queues);
  const queue = queues.get(method) ?? [];
  queue.push(promise);
  queues.set(method, queue);
  return { release, fail };
}

/**
 * Called by a fake API handler for `method`: waits while a deferral holds
 * this call and returns at once when none does.
 */
export async function awaitDeferredCall(
  api: FakeApi,
  method: string,
): Promise<void> {
  const next = deferrals.get(api)?.get(method)?.shift();
  if (next !== undefined) await next;
}
