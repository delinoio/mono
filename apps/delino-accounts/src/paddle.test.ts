// SPDX-License-Identifier: Apache-2.0
import { beforeEach, describe, expect, rs, test } from "@rstest/core";
import { PaddleEnvironment } from "./config.ts";
import { createPaddleCheckout } from "./paddle.tsx";

const paddle = {
  options: [] as Record<string, unknown>[],
  opened: [] as unknown[],
  fail: false,
};

rs.mock("@paddle/paddle-js", () => ({
  initializePaddle: async (options: Record<string, unknown>) => {
    paddle.options.push(options);
    if (paddle.fail) return undefined;
    return {
      Checkout: {
        open: (input: unknown) => paddle.opened.push(input),
      },
    };
  },
}));

beforeEach(() => {
  paddle.options.length = 0;
  paddle.opened.length = 0;
  paddle.fail = false;
});

describe("createPaddleCheckout", () => {
  test("loads Paddle.js once with the configured token and opens the transaction", async () => {
    const checkout = createPaddleCheckout({
      clientToken: "test_token",
      environment: PaddleEnvironment.Sandbox,
    });
    let closed = 0;
    await checkout.open("txn_1", () => closed++);
    await checkout.open("txn_2", () => closed++);
    expect(paddle.options).toHaveLength(1);
    expect(paddle.options[0]).toEqual(
      expect.objectContaining({ token: "test_token", environment: "sandbox" }),
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

  test("fails and retries the load when Paddle.js does not initialize", async () => {
    paddle.fail = true;
    const checkout = createPaddleCheckout({
      clientToken: "test_token",
      environment: PaddleEnvironment.Sandbox,
    });
    await expect(checkout.open("txn_1", () => {})).rejects.toThrow(
      "Paddle checkout could not load",
    );
    paddle.fail = false;
    await checkout.open("txn_1", () => {});
    expect(paddle.options).toHaveLength(2);
    expect(paddle.opened).toEqual([{ transactionId: "txn_1" }]);
  });
});
