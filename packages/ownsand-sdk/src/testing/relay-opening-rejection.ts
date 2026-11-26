// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { Code, ConnectError } from "@connectrpc/connect";
import { SANDBOX, setup } from "./harness.ts";

const methods = ["exec", "attachExec", "readFile"] as const;

for (const method of methods) {
  test(`${method}: missing organization fails locally on a delayed read`, async () => {
    const { api, client } = setup({ organizationId: undefined });
    const request = { sandboxId: SANDBOX };
    client.relay[method](request);
    const delayed = client.relay[method](request)[Symbol.asyncIterator]();
    await setImmediate();

    await assert.rejects(delayed.next(), {
      name: "ConnectError",
      code: Code.InvalidArgument,
      message: /needs an organization/,
    });
    assert.equal(api.fetchedUrls.length, 0);
  });

  test(`${method}: a delayed read gets the same fetch error as an immediate read`, async () => {
    const cause = new TypeError("fetch failed before response headers");
    let fetchCalls = 0;
    const { client } = setup({
      fetch: async () => {
        fetchCalls++;
        throw cause;
      },
    });
    const request = { sandboxId: SANDBOX };
    const open = () => client.relay[method](request)[Symbol.asyncIterator]();
    const immediateError: unknown = await open()
      .next()
      .catch((error) => error);
    assert.ok(immediateError instanceof ConnectError);
    assert.equal(immediateError.cause, cause);

    open();
    const delayed = open();
    await setImmediate();
    assert.equal(fetchCalls, 3);

    await assert.rejects(delayed.next(), (error: unknown) => {
      assert.ok(error instanceof ConnectError);
      assert.equal(error.code, immediateError.code);
      assert.equal(error.rawMessage, immediateError.rawMessage);
      assert.equal(error.cause, cause);
      return true;
    });
  });
}
