// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OwnSandClient } from "./client.ts";
import { ActivityRegistry, createApiTransport } from "./connection.ts";
import {
  classifyFailure,
  FailureClass,
  OwnSandConfigError,
  OwnSandCredentialError,
} from "./errors.ts";
import { BaseImage, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { API_KEY, ORG, SANDBOX, setup } from "./testing/harness.ts";

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

/** Every text of an error and its cause chain: name, message and stack. */
function chainText(error: unknown): string {
  const parts: string[] = [];
  for (let e = error, i = 0; e instanceof Error && i < 10; i++) {
    parts.push(e.name, e.message, String(e.stack));
    e = e.cause;
  }
  return parts.join("\n");
}

describe("an access token that is no header value", () => {
  // Each value holds the sentinel parts on both sides of the bad character.
  const tokens: [string, string][] = [
    ["a line feed", "sentinel-head\nsentinel-tail"],
    ["a carriage return and line feed", "sentinel-head\r\nsentinel-tail"],
    ["a NUL", "sentinel-head\0sentinel-tail"],
    ["a character above Latin-1", "sentinel-head\u20acsentinel-tail"],
  ];

  test.each(tokens)(
    "with %s ends a unary call without a request and without the token in the error",
    async (_name, token) => {
      const { h, client } = tokenSetup(() => token);
      h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
      const error: any = await h.clock
        .run(client.sandbox(SANDBOX).get())
        .catch((e) => e);
      expect(error).toBeInstanceOf(Error);
      expect(error.message).toContain("accessToken returned an invalid token");
      const text = chainText(error);
      expect(text).not.toContain("sentinel");
      expect(classifyFailure(error)).toBe(FailureClass.Terminal);
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test.each(tokens)(
    "with %s ends a streaming call the same way",
    async (_name, token) => {
      const h = setup();
      const transport = createApiTransport({
        origin: h.api.origin,
        credential: { accessToken: () => token },
        organizationId: ORG,
        fetch: h.api.fetch,
        logger: undefined,
        activity: new ActivityRegistry(),
        now: () => 0,
      });
      h.api.stream(RelayService, "Exec", function* () {});
      const error: any = await transport
        .stream(
          RelayService.method.exec,
          undefined,
          undefined,
          undefined,
          (async function* () {
            yield create(RelayService.method.exec.input, {});
          })(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(Error);
      expect(chainText(error)).not.toContain("sentinel");
      expect(classifyFailure(error)).toBe(FailureClass.Terminal);
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test("ends createSandbox and a relay operation after one callback call", async () => {
    let calls = 0;
    const { h, client } = tokenSetup(() => {
      calls++;
      return "sentinel-head\nsentinel-tail";
    });
    const started = h.clock.now();
    const created: any = await h.clock
      .run(client.createSandbox(validCreate))
      .catch((e) => e);
    const read: any = await h.clock
      .run(client.sandbox(SANDBOX).readFileBytes("/data"))
      .catch((e) => e);
    expect(chainText(created)).not.toContain("sentinel");
    expect(chainText(read)).not.toContain("sentinel");
    expect(calls).toBe(2);
    expect(h.api.calls).toHaveLength(0);
    expect(h.clock.now() - started).toBeLessThan(1000);
  });

  test("keeps API key redaction", async () => {
    const h = setup();
    h.api.unary(SandboxService, "GetSandbox", () => {
      throw new ConnectError(`bad key ${API_KEY}`, Code.Unauthenticated);
    });
    const error: any = await h.client
      .sandbox(SANDBOX)
      .get()
      .catch((e) => e);
    expect(chainText(error)).not.toContain(API_KEY);
    expect(error.message).toContain("[redacted]");
  });

  test("still sends a token with only edge whitespace and redacts its trimmed form", async () => {
    const { h, client } = tokenSetup(() => "sentinel-edge \n");
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => {
      throw new ConnectError(
        `rejected ${call.headers.get("authorization")}`,
        Code.Unauthenticated,
      );
    });
    const error: any = await client
      .sandbox(SANDBOX)
      .get()
      .catch((e) => e);
    expect(h.api.calls).toHaveLength(1);
    expect(h.api.calls[0]!.headers.get("authorization")).toBe(
      "Bearer sentinel-edge",
    );
    expect(error.code).toBe(Code.Unauthenticated);
    expect(chainText(error)).not.toContain("sentinel-edge");
  });
});
