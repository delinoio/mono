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
  redactError,
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

const KEY = "sentinel-key-0123456789";

describe("redactError keeps the classification of a rebuilt Connect error", () => {
  function leaky(code = Code.Unknown) {
    const nested = new Error(`nested diagnostic ${KEY}`);
    const cause = new Error(`adapter diagnostic ${KEY}`, { cause: nested });
    const error = new ConnectError(
      `adapter diagnostic ${KEY}`,
      code,
      { "x-trace": "abc" },
      undefined,
      cause,
    );
    return { error, cause };
  }

  test("a transport failure with a cause stays an interruption, also with code unknown", () => {
    const { error, cause } = leaky();
    expect(classifyFailure(error)).toBe(FailureClass.Interruption);
    const redacted = redactError(error, [KEY]) as ConnectError;
    expect(redacted).not.toBe(error);
    expect(redacted).toBeInstanceOf(ConnectError);
    expect(classifyFailure(redacted)).toBe(FailureClass.Interruption);
    expect(redacted.code).toBe(Code.Unknown);
    expect(redacted.rawMessage).toBe("adapter diagnostic [redacted]");
    // The replacement cause is generic and holds no reference to the
    // original cause or to the secret.
    expect(redacted.cause).toBeInstanceOf(Error);
    expect(redacted.cause).not.toBeInstanceOf(ConnectError);
    expect(redacted.cause).not.toBe(cause);
    expect((redacted.cause as Error).message).toBe("transport failure");
    expect((redacted.cause as Error).cause).toBeUndefined();
    expect(chainText(redacted)).not.toContain(KEY);
    // The input stays as it was.
    expect(error.cause).toBe(cause);
    expect(error.rawMessage).toContain(KEY);
  });

  test("keeps metadata and details", () => {
    const { error } = leaky(Code.Unavailable);
    error.details = [];
    const redacted = redactError(error, [KEY]) as ConnectError;
    expect(redacted.metadata.get("x-trace")).toBe("abc");
    expect(redacted.details).toBe(error.details);
    expect(redacted.code).toBe(Code.Unavailable);
    expect(classifyFailure(redacted)).toBe(FailureClass.Interruption);
  });

  test("a cause-free error stays terminal and an unchanged error keeps its identity", () => {
    const bare = new ConnectError(`rejected ${KEY}`, Code.Unknown);
    expect(classifyFailure(bare)).toBe(FailureClass.Terminal);
    const redacted = redactError(bare, [KEY]) as ConnectError;
    expect(redacted.cause).toBeUndefined();
    expect(classifyFailure(redacted)).toBe(FailureClass.Terminal);
    const { error } = leaky();
    expect(redactError(error, ["unrelated-secret"])).toBe(error);
    expect(redactError(error, [])).toBe(error);
  });

  test("a Connect error as cause is not kept and the code decides", () => {
    const inner = new ConnectError(`inner ${KEY}`, Code.Unavailable);
    const outer = new ConnectError(
      `outer ${KEY}`,
      Code.Unavailable,
      undefined,
      undefined,
      inner,
    );
    const redacted = redactError(outer, [KEY]) as ConnectError;
    expect(redacted.cause).toBeUndefined();
    expect(classifyFailure(redacted)).toBe(classifyFailure(outer));
    expect(classifyFailure(redacted)).toBe(FailureClass.Unavailable);
  });

  test("a redacted credential failure never becomes retryable", () => {
    const credential = new OwnSandCredentialError(`token source ${KEY}`);
    const wrapped = new ConnectError(
      `token source ${KEY}`,
      Code.Unknown,
      undefined,
      undefined,
      credential,
    );
    expect(classifyFailure(wrapped)).toBe(FailureClass.Terminal);
    const redacted = redactError(wrapped, [KEY]) as ConnectError;
    expect(redacted.cause).toBeInstanceOf(OwnSandCredentialError);
    expect(redacted.cause).not.toBe(credential);
    expect((redacted.cause as Error).cause).toBeUndefined();
    expect(classifyFailure(redacted)).toBe(FailureClass.Terminal);
    expect(chainText(redacted)).not.toContain(KEY);
    // A bare credential error keeps its class when its message changes.
    const bare = redactError(credential, [KEY]);
    expect(bare).toBeInstanceOf(OwnSandCredentialError);
    expect(classifyFailure(bare)).toBe(FailureClass.Terminal);
  });
});

describe("transport failures keep their classification through the client redaction", () => {
  const protoHeaders = (type: string) => ({ "content-type": type });
  const failingBody = () =>
    new ReadableStream<Uint8Array>({
      start(controller) {
        controller.error(new Error(`body failure ${KEY}`));
      },
    });
  // The adapter fails with a plain error, or with a Connect error that
  // already wraps the original error as its cause (a Connect-aware adapter).
  const failures: [string, () => Error][] = [
    ["a plain error", () => new Error(`adapter diagnostic ${KEY}`)],
    [
      "a Connect error with a cause",
      () => ConnectError.from(new Error(`adapter diagnostic ${KEY}`)),
    ],
  ];
  const credentials = [
    ["an API key", { apiKey: KEY }],
    ["an access token", { accessToken: () => KEY }],
  ] as const;

  describe.each(credentials)("with %s", (_name, credential) => {
    const transportOf = (fetch: typeof globalThis.fetch) =>
      createApiTransport({
        origin: setup().api.origin,
        credential,
        organizationId: ORG,
        fetch,
        logger: undefined,
        activity: new ActivityRegistry(),
        now: () => 0,
      });
    const unary = (transport: ReturnType<typeof transportOf>) =>
      transport.unary(
        SandboxService.method.getSandbox,
        undefined,
        undefined,
        undefined,
        { sandboxId: SANDBOX },
      );
    const stream = (transport: ReturnType<typeof transportOf>) =>
      transport.stream(
        RelayService.method.exec,
        undefined,
        undefined,
        undefined,
        (async function* () {
          yield create(RelayService.method.exec.input, {});
        })(),
      );

    test.each(failures)(
      "unary: a failed fetch (%s) stays an interruption without the secret",
      async (_kind, fail) => {
        const transport = transportOf(async () => {
          throw fail();
        });
        const error: any = await unary(transport).catch((e) => e);
        expect(error).toBeInstanceOf(ConnectError);
        expect(error.message).toContain("[redacted]");
        expect(chainText(error)).not.toContain(KEY);
        expect(error.cause).toBeDefined();
        expect(classifyFailure(error)).toBe(FailureClass.Interruption);
      },
    );

    test("unary: a failed response body read stays an interruption without the secret", async () => {
      const transport = transportOf(
        async () =>
          new Response(failingBody(), {
            status: 200,
            headers: protoHeaders("application/proto"),
          }),
      );
      const error: any = await unary(transport).catch((e) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect(chainText(error)).not.toContain(KEY);
      expect(classifyFailure(error)).toBe(FailureClass.Interruption);
    });

    test.each(failures)(
      "stream: a failed fetch (%s) stays an interruption without the secret",
      async (_kind, fail) => {
        const transport = transportOf(async () => {
          throw fail();
        });
        const error: any = await stream(transport).catch((e) => e);
        expect(error).toBeInstanceOf(ConnectError);
        expect(chainText(error)).not.toContain(KEY);
        expect(classifyFailure(error)).toBe(FailureClass.Interruption);
      },
    );

    test("stream: a failed response body read stays an interruption without the secret", async () => {
      const transport = transportOf(
        async () =>
          new Response(failingBody(), {
            status: 200,
            headers: protoHeaders("application/connect+proto"),
          }),
      );
      const res = await stream(transport);
      const error: any = await (async () => {
        try {
          for await (const _ of res.message) {
            // Drain until the stream error.
          }
        } catch (e) {
          return e;
        }
        return undefined;
      })();
      expect(error).toBeInstanceOf(ConnectError);
      expect(chainText(error)).not.toContain(KEY);
      expect(classifyFailure(error)).toBe(FailureClass.Interruption);
    });
  });

  const credentialsAndFailures = credentials.flatMap(([name, credential]) =>
    failures.map(([kind, fail]) => [name, kind, credential, fail] as const),
  );
  test.each(credentialsAndFailures)(
    "createSandbox with %s retries a one-off adapter failure (%s) with the same idempotency key",
    async (_name, _kind, credential, fail) => {
      const h = setup();
      let failed = 0;
      const client = new OwnSandClient({
        origin: h.api.origin,
        ...credential,
        organizationId: ORG,
        clock: h.clock,
        webrtc: false,
        env: {},
        fetch: async (input, init) => {
          const url = typeof input === "string" ? input : String(input);
          if (url.endsWith("/CreateSandbox") && failed++ === 0) {
            throw fail();
          }
          return h.api.fetch(input, init);
        },
      });
      h.api.unary(SandboxService, "CreateSandbox", () => ({
        sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
      }));
      const sandbox = await h.clock.run(client.createSandbox(validCreate));
      expect(sandbox.sandboxId).toBe(SANDBOX);
      expect(failed).toBe(2);
      expect(h.api.callsOf("CreateSandbox")).toHaveLength(1);
    },
  );

  test.each(failures)(
    "createSandbox ends after its attempts when every adapter call fails (%s), without the secret",
    async (_kind, fail) => {
      const h = setup();
      const client = new OwnSandClient({
        origin: h.api.origin,
        apiKey: KEY,
        organizationId: ORG,
        clock: h.clock,
        webrtc: false,
        env: {},
        fetch: async () => {
          throw fail();
        },
      });
      const error: any = await h.clock
        .run(client.createSandbox(validCreate))
        .catch((e) => e);
      expect(chainText(error)).not.toContain(KEY);
      expect(classifyFailure(error)).toBe(FailureClass.Interruption);
    },
  );
});
