// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { PaddleEnvironment } from "./config.ts";
import { createPaddleCheckout } from "./paddle.tsx";

// These tests drive the real `@paddle/paddle-js` module and the real script
// injection: no SDK mock. happy-dom reports every script with a `src` as a
// failed load by itself because JavaScript file loading is disabled, which
// would race with the events of the test. So `document.head.appendChild`
// records the script element instead of connecting it, and the test controls
// the `load` and `error` events and the `PaddleBillingV1` global that the
// Paddle.js script defines.

const SCRIPT_URL = "https://cdn.paddle.com/paddle/v2/paddle.js";

const paddle = {
  environments: [] as string[],
  options: [] as Record<string, unknown>[],
  opened: [] as unknown[],
};

/** Script elements that the checkout inserted, in order. */
let inserted: HTMLScriptElement[] = [];
/** Script elements that the checkout removed again. */
let removed: Set<Element> = new Set();

const originalAppendChild = document.head.appendChild;
const originalRemove = HTMLScriptElement.prototype.remove;

function installPaddleGlobal() {
  (window as unknown as Record<string, unknown>).PaddleBillingV1 = {
    Environment: { set: (name: string) => paddle.environments.push(name) },
    Initialize: (options: Record<string, unknown>) => {
      paddle.options.push(options);
    },
    Checkout: {
      open: (input: unknown) => paddle.opened.push(input),
    },
  };
}

/** Waits until the checkout inserts its next script element, and returns it. */
async function insertedScript(previous: number) {
  for (let i = 0; i < 50 && inserted.length <= previous; i++) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  expect(inserted).toHaveLength(previous + 1);
  const script = inserted[previous]!;
  expect(script.src).toBe(SCRIPT_URL);
  return script;
}

function newCheckout() {
  return createPaddleCheckout({
    clientToken: "test_token",
    environment: PaddleEnvironment.Sandbox,
  });
}

beforeEach(() => {
  paddle.environments.length = 0;
  paddle.options.length = 0;
  paddle.opened.length = 0;
  inserted = [];
  removed = new Set();
  delete (window as unknown as Record<string, unknown>).PaddleBillingV1;
  document.head.appendChild = function <T extends Node>(node: T): T {
    if (node instanceof HTMLScriptElement) inserted.push(node);
    return node;
  };
  HTMLScriptElement.prototype.remove = function (this: HTMLScriptElement) {
    removed.add(this);
  };
});

afterEach(() => {
  document.head.appendChild = originalAppendChild;
  HTMLScriptElement.prototype.remove = originalRemove;
  delete (window as unknown as Record<string, unknown>).PaddleBillingV1;
});

describe("createPaddleCheckout", () => {
  test("loads Paddle.js once with the configured token and opens the transaction", async () => {
    const checkout = newCheckout();
    let closed = 0;
    const first = checkout.open("txn_1", () => closed++);
    const script = await insertedScript(0);
    installPaddleGlobal();
    script.dispatchEvent(new Event("load"));
    await first;
    await checkout.open("txn_2", () => closed++);
    expect(inserted).toHaveLength(1);
    expect(paddle.environments).toEqual(["sandbox"]);
    expect(paddle.options).toHaveLength(1);
    expect(paddle.options[0]).toEqual(
      expect.objectContaining({ token: "test_token" }),
    );
    expect(paddle.opened).toEqual([
      { transactionId: "txn_1" },
      { transactionId: "txn_2" },
    ]);
    const callback = paddle.options[0]!.eventCallback as (event: {
      name?: string;
    }) => void;
    callback({ name: "checkout.loaded" });
    expect(closed).toBe(0);
    callback({ name: "checkout.closed" });
    callback({ name: "checkout.completed" });
    expect(closed).toBe(2);
  });

  test("loads the script again after a script error and opens the transaction", async () => {
    const checkout = newCheckout();
    const failed = checkout.open("txn_1", () => {});
    const failedAssertion = expect(failed).rejects.toThrow(
      "Failed to load Paddle.js",
    );
    const firstScript = await insertedScript(0);
    firstScript.dispatchEvent(new Event("error"));
    await failedAssertion;
    // The failed element is removed, so the retry cannot reuse it.
    expect(removed.has(firstScript)).toBe(true);
    expect(paddle.options).toHaveLength(0);

    const retry = checkout.open("txn_1", () => {});
    const secondScript = await insertedScript(1);
    expect(secondScript).not.toBe(firstScript);
    installPaddleGlobal();
    secondScript.dispatchEvent(new Event("load"));
    await retry;
    expect(paddle.opened).toEqual([{ transactionId: "txn_1" }]);
    expect(paddle.options).toHaveLength(1);

    // A third attempt reuses the initialization: no script, no Initialize.
    await checkout.open("txn_2", () => {});
    expect(inserted).toHaveLength(2);
    expect(paddle.options).toHaveLength(1);
    expect(paddle.opened).toEqual([
      { transactionId: "txn_1" },
      { transactionId: "txn_2" },
    ]);
  });

  test("fails and retries the load when the script defines no Paddle global", async () => {
    const checkout = newCheckout();
    const failed = checkout.open("txn_1", () => {});
    const failedAssertion = expect(failed).rejects.toThrow(
      "Paddle checkout could not load",
    );
    const script = await insertedScript(0);
    script.dispatchEvent(new Event("load"));
    await failedAssertion;

    const retry = checkout.open("txn_1", () => {});
    const secondScript = await insertedScript(1);
    installPaddleGlobal();
    secondScript.dispatchEvent(new Event("load"));
    await retry;
    expect(paddle.opened).toEqual([{ transactionId: "txn_1" }]);
  });

  test("shares one script load between concurrent attempts", async () => {
    const checkout = newCheckout();
    const a = checkout.open("txn_1", () => {});
    const b = checkout.open("txn_2", () => {});
    const script = await insertedScript(0);
    installPaddleGlobal();
    script.dispatchEvent(new Event("load"));
    await Promise.all([a, b]);
    expect(inserted).toHaveLength(1);
    expect(paddle.options).toHaveLength(1);
  });

  describe("when the caller aborts", () => {
    function eventCallback() {
      return paddle.options[0]!.eventCallback as (event: {
        name?: string;
      }) => void;
    }

    test("opens nothing and installs no callback when the signal aborts while Paddle.js loads", async () => {
      const checkout = newCheckout();
      const controller = new AbortController();
      let staleClosed = 0;
      const stale = checkout.open(
        "txn_stale",
        () => staleClosed++,
        controller.signal,
      );
      const script = await insertedScript(0);
      controller.abort();
      installPaddleGlobal();
      script.dispatchEvent(new Event("load"));
      await stale;
      expect(paddle.opened).toEqual([]);
      // The shared load finished and initialized Paddle.js once.
      expect(paddle.options).toHaveLength(1);
      eventCallback()({ name: "checkout.closed" });
      expect(staleClosed).toBe(0);

      // A later valid attempt reuses the loader and opens its transaction.
      let closed = 0;
      await checkout.open("txn_current", () => closed++);
      expect(inserted).toHaveLength(1);
      expect(paddle.options).toHaveLength(1);
      expect(paddle.opened).toEqual([{ transactionId: "txn_current" }]);
      eventCallback()({ name: "checkout.closed" });
      expect(closed).toBe(1);
      expect(staleClosed).toBe(0);
    });

    test("does not replace the callback of the open checkout with a stale attempt", async () => {
      const checkout = newCheckout();
      let currentClosed = 0;
      const current = checkout.open("txn_current", () => currentClosed++);
      const controller = new AbortController();
      let staleClosed = 0;
      const stale = checkout.open(
        "txn_stale",
        () => staleClosed++,
        controller.signal,
      );
      const script = await insertedScript(0);
      controller.abort();
      installPaddleGlobal();
      script.dispatchEvent(new Event("load"));
      await Promise.all([current, stale]);
      expect(paddle.opened).toEqual([{ transactionId: "txn_current" }]);
      eventCallback()({ name: "checkout.completed" });
      expect(currentClosed).toBe(1);
      expect(staleClosed).toBe(0);
    });

    test("keeps the loader available after a stale attempt whose load failed", async () => {
      const checkout = newCheckout();
      const controller = new AbortController();
      const stale = checkout.open("txn_stale", () => {}, controller.signal);
      const staleAssertion = expect(stale).rejects.toThrow(
        "Failed to load Paddle.js",
      );
      const first = await insertedScript(0);
      controller.abort();
      first.dispatchEvent(new Event("error"));
      await staleAssertion;

      const retry = checkout.open("txn_current", () => {});
      const second = await insertedScript(1);
      installPaddleGlobal();
      second.dispatchEvent(new Event("load"));
      await retry;
      expect(paddle.opened).toEqual([{ transactionId: "txn_current" }]);
    });
  });
});
