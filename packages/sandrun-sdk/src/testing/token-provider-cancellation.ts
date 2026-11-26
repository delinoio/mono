// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { Code } from "@connectrpc/connect";
import { SandRunClient } from "../index.ts";
import { FAKE_ORIGIN, fakeServer } from "./fake-server.ts";

function startCall(
  controller: AbortController,
  token: () => string | Promise<string>,
) {
  const server = fakeServer(() => {});
  const client = new SandRunClient({
    origin: FAKE_ORIGIN,
    fetch: server.fetch,
    token,
  });
  const result = client.organizations.getOrganization(
    { organizationId: "0193a000-0000-7000-8000-000000000001" },
    { signal: controller.signal },
  );
  return { result, calls: server.calls };
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
    const token = Promise.withResolvers<string>();
    const providerStarted = Promise.withResolvers<void>();
    const { result, calls } = startCall(controller, () => {
      providerStarted.resolve();
      return token.promise;
    });
    await providerStarted.promise;
    const canceled = assert.rejects(result, {
      name: "ConnectError",
      code: Code.Canceled,
    });
    controller.abort();
    await canceled;

    if (mode === "resolve") {
      token.resolve("late-token");
    } else {
      token.reject(new Error("provider failed after pending abort"));
    }
    await setImmediate();
    assert.deepEqual(calls, []);
  });
}
