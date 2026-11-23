// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OwnSandClient } from "./client.ts";
import {
  classifyFailure,
  FailureClass,
  OwnSandCredentialError,
} from "./errors.ts";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { ORG, SANDBOX, setup } from "./testing/harness.ts";

const FALLBACK = "accessToken callback failed";

/** A client that authenticates with an access token callback. */
function tokenSetup(accessToken: () => string | Promise<string>) {
  const h = setup();
  h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
  const client = new OwnSandClient({
    origin: h.api.origin,
    fetch: h.api.fetch,
    clock: h.clock,
    env: {},
    organizationId: ORG,
    webrtc: false,
    accessToken,
  });
  return { h, client };
}

/** A rejection value whose formatting with String() throws. */
function unformattable(): object {
  return Object.create(null);
}

/** An Error whose message getter throws. */
function unreadableMessage(): Error {
  const error = new Error("hidden");
  Object.defineProperty(error, "message", {
    get() {
      throw new TypeError("message getter failed");
    },
  });
  return error;
}

/** Records the unhandled rejections of the process while a body runs. */
async function collectUnhandled(body: () => Promise<void>): Promise<unknown[]> {
  const unhandled: unknown[] = [];
  const onUnhandled = (e: unknown) => unhandled.push(e);
  process.on("unhandledRejection", onUnhandled);
  try {
    await body();
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
  return unhandled;
}

describe("an access token callback failure that cannot be formatted", () => {
  const cases: [string, () => string | Promise<string>][] = [
    [
      "an async rejection without a prototype",
      () => Promise.reject(unformattable()),
    ],
    [
      "a synchronous throw without a prototype",
      () => {
        throw unformattable();
      },
    ],
    [
      "an async rejection of an Error with a throwing message getter",
      () => Promise.reject(unreadableMessage()),
    ],
    [
      "a synchronous throw of an Error with a throwing message getter",
      () => {
        throw unreadableMessage();
      },
    ],
  ];

  test.each(cases)(
    "ends the call with a credential error after %s",
    async (_name, callback) => {
      const { h, client } = tokenSetup(callback);
      let error: any;
      const unhandled = await collectUnhandled(async () => {
        error = await h.clock
          .run(client.sandbox(SANDBOX).get())
          .catch((e) => e);
        await h.clock.settle();
      });
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.Unknown);
      expect(error.cause).toBeInstanceOf(OwnSandCredentialError);
      expect(error.message).toContain(FALLBACK);
      expect(classifyFailure(error)).toBe(FailureClass.Terminal);
      expect(h.api.calls).toHaveLength(0);
      expect(unhandled).toEqual([]);
    },
  );

  test("keeps the original rejection value as the cause", async () => {
    const value = unformattable();
    const { h, client } = tokenSetup(() => Promise.reject(value));
    const error: any = await h.clock
      .run(client.sandbox(SANDBOX).get())
      .catch((e) => e);
    expect(error.cause.cause).toBe(value);
  });
});

describe("an access token callback failure that can be formatted", () => {
  test("keeps the message of an ordinary Error", async () => {
    const { h, client } = tokenSetup(() =>
      Promise.reject(new Error("token source unavailable")),
    );
    const error: any = await h.clock
      .run(client.sandbox(SANDBOX).get())
      .catch((e) => e);
    expect(error.cause).toBeInstanceOf(OwnSandCredentialError);
    expect(error.message).toContain("token source unavailable");
    expect(error.message).not.toContain(FALLBACK);
  });

  test("formats a non-Error value with String()", async () => {
    const { h, client } = tokenSetup(() => Promise.reject("plain text"));
    const error: any = await h.clock
      .run(client.sandbox(SANDBOX).get())
      .catch((e) => e);
    expect(error.message).toContain("plain text");
  });
});

describe("a cancellation while the access token callback is pending", () => {
  test("keeps the abort reason and swallows a later unformattable rejection", async () => {
    let rejectToken!: (error: unknown) => void;
    const { h, client } = tokenSetup(
      () => new Promise<string>((_, reject) => (rejectToken = reject)),
    );
    const abort = new AbortController();
    const reason = new Error("caller gave up");
    let error: any;
    const unhandled = await collectUnhandled(async () => {
      const outcome = h.clock
        .run(client.sandbox(SANDBOX).get(abort.signal))
        .catch((e) => e);
      await h.clock.settle();
      abort.abort(reason);
      error = await outcome;
      rejectToken(unformattable());
      await h.clock.settle();
    });
    expect(error).not.toBeInstanceOf(OwnSandCredentialError);
    expect(error?.cause).not.toBeInstanceOf(OwnSandCredentialError);
    expect(h.api.calls).toHaveLength(0);
    expect(unhandled).toEqual([]);
  });
});
