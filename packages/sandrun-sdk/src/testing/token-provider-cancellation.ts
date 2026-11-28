// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { Code } from "@connectrpc/connect";
import { SandRunClient } from "../index.ts";
import { FAKE_ORIGIN } from "./fake-server.ts";

function startCall(
  controller: AbortController,
  token: () => string | Promise<string>,
) {
  const calls: {
    aborted: boolean | undefined;
    authorization: string | null;
  }[] = [];
  const client = new SandRunClient({
    origin: FAKE_ORIGIN,
    fetch: async (_input, init) => {
      calls.push({
        aborted: init?.signal?.aborted,
        authorization: new Headers(init?.headers).get("Authorization"),
      });
      init?.signal?.throwIfAborted();
      return new Response(null, {
        headers: { "Content-Type": "application/proto" },
      });
    },
    token,
  });
  const result = client.organizations.getOrganization(
    { organizationId: "0193a000-0000-7000-8000-000000000001" },
    { signal: controller.signal },
  );
  return { result, calls };
}

function startDeferredCall(controller: AbortController) {
  const token = Promise.withResolvers<string>();
  const providerStarted = Promise.withResolvers<void>();
  const call = startCall(controller, () => {
    providerStarted.resolve();
    return token.promise;
  });
  return {
    ...call,
    providerStarted: providerStarted.promise,
    resolveToken: token.resolve,
    rejectToken: token.reject,
  };
}

for (const mode of ["throw", "reject"] as const) {
  test(`token provider aborts synchronously and then fails (${mode})`, async () => {
    const controller = new AbortController();
    const { result, calls } = startCall(controller, () => {
      controller.abort();
      const error = new Error("provider failed after synchronous abort");
      if (mode === "throw") {
        throw error;
      }
      return Promise.reject(error);
    });
    await assert.rejects(result, { name: "ConnectError", code: Code.Canceled });
    await setImmediate();
    assert.deepEqual(calls, []);
  });
}

for (const mode of ["resolve", "reject"] as const) {
  test(`pending token provider settles after abort (${mode})`, async () => {
    const controller = new AbortController();
    const { result, calls, providerStarted, resolveToken, rejectToken } =
      startDeferredCall(controller);
    await providerStarted;
    const canceled = assert.rejects(result, {
      name: "ConnectError",
      code: Code.Canceled,
    });
    controller.abort();
    await canceled;

    if (mode === "resolve") {
      resolveToken("late-token");
    } else {
      rejectToken(new Error("provider failed after pending abort"));
    }
    await setImmediate();
    assert.deepEqual(calls, []);
  });
}

test("aborting after token resolution skips fetch", async () => {
  const controller = new AbortController();
  const { result, calls, providerStarted, resolveToken } =
    startDeferredCall(controller);
  await providerStarted;
  resolveToken("fixture-token");
  // Abort after the token wait accepts the result, before the interceptor resumes.
  queueMicrotask(() => queueMicrotask(() => controller.abort()));

  await assert.rejects(result, { name: "ConnectError", code: Code.Canceled });
  assert.deepEqual(calls, []);
});

test("a resolved token reaches fetch when the call is active", async () => {
  const controller = new AbortController();
  const { result, calls, providerStarted, resolveToken } =
    startDeferredCall(controller);
  await providerStarted;
  resolveToken("fixture-token");

  await result;
  assert.deepEqual(calls, [
    { aborted: false, authorization: "Bearer fixture-token" },
  ]);
});
