// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OwnSandClient } from "./client.ts";
import {
  classifyFailure,
  FailureClass,
  OwnSandConfigError,
  OwnSandCredentialError,
} from "./errors.ts";
import { BaseImage, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { ORG, SANDBOX, setup } from "./testing/harness.ts";

const HOUR = 3_600_000;

const validCreate = {
  source: { baseImage: BaseImage.LINUX },
  resources: { vcpuCount: 2, memoryMib: 2048, diskGib: 10 },
  idleTtl: HOUR,
  maxTtl: 2 * HOUR,
};

/** A relay-only client that authenticates with an access token callback. */
function tokenSetup(accessToken: () => string | Promise<string>) {
  const h = setup();
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

/** Counts the calls of a callback that fails in the given way. */
function failing(kind: "empty" | "throws" | "rejects") {
  const state = { calls: 0 };
  const callback = (): string | Promise<string> => {
    state.calls++;
    switch (kind) {
      case "empty":
        return "";
      case "throws":
        throw new Error("token source unavailable");
      case "rejects":
        return Promise.reject(new Error("token source unavailable"));
    }
  };
  return { state, callback };
}

describe("a failing access token callback (local credential failure)", () => {
  const operations: [string, (client: OwnSandClient) => Promise<unknown>][] = [
    [
      "exec",
      (c) =>
        c
          .sandbox(SANDBOX)
          .exec({ command: ["true"] })
          .wait(),
    ],
    ["readFileBytes", (c) => c.sandbox(SANDBOX).readFileBytes("/data")],
    ["writeFile", (c) => c.sandbox(SANDBOX).writeFile("/f", "hello")],
  ];

  describe.each([
    ["an empty token", "empty", "accessToken returned no token"],
    ["a throwing callback", "throws", "token source unavailable"],
    ["a rejecting callback", "rejects", "token source unavailable"],
  ] as const)("%s", (_name, kind, message) => {
    test.each(operations)(
      "ends relay %s at once with one callback call and no request",
      async (_op, run) => {
        const { state, callback } = failing(kind);
        const { h, client } = tokenSetup(callback);
        const started = h.clock.now();
        const error: any = await h.clock.run(run(client)).catch((e) => e);
        expect(error).toBeInstanceOf(ConnectError);
        expect(error.cause).toBeInstanceOf(OwnSandCredentialError);
        expect(error.message).toContain(message);
        expect(classifyFailure(error)).toBe(FailureClass.Terminal);
        expect(state.calls).toBe(1);
        expect(h.api.calls).toHaveLength(0);
        expect(h.clock.now() - started).toBeLessThan(1000);
      },
    );

    test("ends createSandbox after one callback call and one attempt", async () => {
      const { state, callback } = failing(kind);
      const { h, client } = tokenSetup(callback);
      h.api.unary(SandboxService, "CreateSandbox", () => ({
        sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
      }));
      const started = h.clock.now();
      const error = await h.clock
        .run(client.createSandbox(validCreate))
        .catch((e) => e);
      expect(error.message).toContain(message);
      expect(state.calls).toBe(1);
      expect(h.api.calls).toHaveLength(0);
      expect(h.clock.now() - started).toBeLessThan(1000);
    });
  });

  test("keeps the empty-token error an OwnSandConfigError", () => {
    expect(new OwnSandCredentialError("x")).toBeInstanceOf(OwnSandConfigError);
  });

  test("classifies a credential error as terminal, bare or as the cause of a Connect error", () => {
    const credential = new OwnSandCredentialError("no token");
    expect(classifyFailure(credential)).toBe(FailureClass.Terminal);
    expect(classifyFailure(ConnectError.from(credential))).toBe(
      FailureClass.Terminal,
    );
    // Controls: other local errors and wrapped transport failures retry.
    expect(classifyFailure(new Error("x"))).toBe(FailureClass.Interruption);
    expect(
      classifyFailure(ConnectError.from(new TypeError("fetch failed"))),
    ).toBe(FailureClass.Interruption);
  });

  test("still retries a real transport interruption of the call", async () => {
    let calls = 0;
    const callback = () => `token-${++calls}`;
    const { h, client } = tokenSetup(callback);
    h.api.unary(SandboxService, "CreateSandbox", (_req, call) => {
      if (call.index === 0) throw new ConnectError("restart", Code.Unavailable);
      return { sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING } };
    });
    await h.clock.run(client.createSandbox(validCreate));
    expect(h.api.callsOf("CreateSandbox")).toHaveLength(2);
  });

  test("leaves a working callback unaffected", async () => {
    const { h, client } = tokenSetup(() => "good-token");
    h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
    await client.sandbox(SANDBOX).get();
    expect(h.api.calls[0]!.headers.get("authorization")).toBe(
      "Bearer good-token",
    );
  });
});
