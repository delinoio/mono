// SPDX-License-Identifier: Apache-2.0
import { getEventListeners } from "node:events";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { type CreateSandboxInput, OwnSandClient } from "./client.ts";
import {
  ActivityRegistry,
  createApiTransport,
  DEFAULT_API_ORIGIN,
  guardedFetch,
  resolveApiOrigin,
} from "./connection.ts";
import { OwnSandConfigError, OwnSandValidationError } from "./errors.ts";
import {
  BaseImage,
  BillingReason,
  DataPathTransport,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { ExecResultKind } from "./operations/exec.ts";
import { type LogRecord } from "./logger.ts";
import { execBinding, requestDigest } from "./digest.ts";
import { isUuidV7 } from "./uuidv7.ts";
import { Hang, LostResponse, ownsandError } from "./testing/fake-api.ts";
import {
  API_KEY,
  ev,
  execMsg,
  file,
  ORG,
  SANDBOX,
  setup,
} from "./testing/harness.ts";

const HOUR = 3_600_000;

const validCreate = {
  source: { baseImage: BaseImage.LINUX },
  resources: { vcpuCount: 2, memoryMib: 2048, diskGib: 10 },
  idleTtl: HOUR,
  maxTtl: 2 * HOUR,
};

describe("client construction", () => {
  test("reads OWNSAND_API_KEY and OWNSAND_ORG without explicit values", async () => {
    const h = setup();
    h.api.unary(SandboxService, "GetSandbox", (req) => ({
      sandbox: { sandboxId: req.sandboxId, state: SandboxState.RUNNING },
    }));
    const client = new OwnSandClient({
      origin: h.api.origin,
      fetch: h.api.fetch,
      clock: h.clock,
      env: { OWNSAND_API_KEY: "osk_from_env", OWNSAND_ORG: ORG },
    });
    await client.sandbox(SANDBOX).get();
    const [call] = h.api.callsOf("GetSandbox");
    expect(call!.headers.get("authorization")).toBe("Bearer osk_from_env");
    expect((call!.request as any).organizationId).toBe(ORG);
  });

  test("fails without any credential", () => {
    expect(() => new OwnSandClient({ env: {} })).toThrow(OwnSandConfigError);
  });

  test("rejects a whitespace-only explicit API key", () => {
    expect(
      () =>
        new OwnSandClient({ apiKey: "   ", env: { OWNSAND_API_KEY: API_KEY } }),
    ).toThrow(new OwnSandConfigError("apiKey must not be empty"));
  });

  test("treats a whitespace-only OWNSAND_API_KEY as unset", () => {
    const env = { OWNSAND_API_KEY: "   " };
    expect(() => new OwnSandClient({ env })).toThrow(
      new OwnSandConfigError(
        "no credential: pass apiKey or accessToken, or set OWNSAND_API_KEY",
      ),
    );
  });

  test("uses an explicit API key when OWNSAND_API_KEY contains only whitespace", async () => {
    const h = setup({ apiKey: "osk_valid", env: { OWNSAND_API_KEY: "   " } });
    await h.client.sandbox(SANDBOX).get();
    const [call] = h.api.callsOf("GetSandbox");
    expect(call!.headers.get("authorization")).toBe("Bearer osk_valid");
  });

  test("uses the production origin by default and rejects origins with a path or credentials", () => {
    expect(resolveApiOrigin(undefined)).toBe(DEFAULT_API_ORIGIN);
    expect(resolveApiOrigin("http://127.0.0.1:8300/")).toBe(
      "http://127.0.0.1:8300",
    );
    expect(() => resolveApiOrigin("https://a.test/api")).toThrow(
      OwnSandConfigError,
    );
    expect(() => resolveApiOrigin("https://u:p@a.test")).toThrow(
      OwnSandConfigError,
    );
  });

  test("gets a current Logto access token for every call", async () => {
    const h = setup();
    let n = 0;
    const client = new OwnSandClient({
      origin: h.api.origin,
      fetch: h.api.fetch,
      clock: h.clock,
      env: {},
      accessToken: async () => `token-${++n}`,
    });
    h.api.unary(OrganizationService, "ListOrganizations", () => ({}));
    await client.organizations.listOrganizations({});
    await client.organizations.listOrganizations({});
    expect(
      h.api
        .callsOf("ListOrganizations")
        .map((c) => c.headers.get("authorization")),
    ).toEqual(["Bearer token-1", "Bearer token-2"]);
  });
});

describe("organization", () => {
  test("fills the default organization and lets a call override it", async () => {
    const h = setup();
    h.api.unary(MemberService, "ListMembers", () => ({}));
    await h.client.members.listMembers({});
    await h.client.members.listMembers({ organizationId: "other-org" });
    expect(
      h.api
        .callsOf("ListMembers")
        .map((c) => (c.request as any).organizationId),
    ).toEqual([ORG, "other-org"]);
  });

  test("fails before sending when an RPC needs an organization that is not set", async () => {
    const h = setup({ organizationId: undefined });
    h.api.unary(MemberService, "ListMembers", () => ({}));
    const error = await h.client.members.listMembers({}).catch((e) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect(error.code).toBe(Code.InvalidArgument);
    expect(h.api.calls).toHaveLength(0);
    expect(() => h.client.sandbox(SANDBOX)).toThrow(OwnSandConfigError);
  });

  test("does not need an organization for organization-less RPCs", async () => {
    const h = setup({ organizationId: undefined });
    h.api.unary(OrganizationService, "ListOrganizations", () => ({}));
    await h.client.organizations.listOrganizations({});
    expect(h.api.callsOf("ListOrganizations")).toHaveLength(1);
  });

  // Server-streaming relay calls carry their single request in an async
  // iterable, which the unary path does not see.
  describe.each([
    {
      method: "Exec",
      open: (h: ReturnType<typeof setup>, organizationId?: string) =>
        h.client.relay.exec({ sandboxId: SANDBOX, organizationId }),
    },
    {
      method: "AttachExec",
      open: (h: ReturnType<typeof setup>, organizationId?: string) =>
        h.client.relay.attachExec({ sandboxId: SANDBOX, organizationId }),
    },
    {
      method: "ReadFile",
      open: (h: ReturnType<typeof setup>, organizationId?: string) =>
        h.client.relay.readFile({ sandboxId: SANDBOX, organizationId }),
    },
  ])("relay $method", ({ method, open }) => {
    async function drain(call: AsyncIterable<unknown>): Promise<void> {
      for await (const _ of call) {
        // The fake ends the stream without a message.
      }
    }

    function fakeStream(h: ReturnType<typeof setup>) {
      h.api.stream(RelayService, method, function* () {});
    }

    function organizationsSent(h: ReturnType<typeof setup>): string[] {
      return h.api
        .callsOf(method)
        .map(
          (c) =>
            (c.request as unknown as { organizationId: string }).organizationId,
        );
    }

    test("fills the client organization when the request omits it or leaves it empty", async () => {
      const h = setup();
      fakeStream(h);
      await drain(open(h));
      await drain(open(h, ""));
      expect(organizationsSent(h)).toEqual([ORG, ORG]);
    });

    test("fills the OWNSAND_ORG organization when the client has no organization", async () => {
      const h = setup({
        organizationId: undefined,
        env: { OWNSAND_ORG: ORG },
      });
      fakeStream(h);
      await drain(open(h));
      expect(organizationsSent(h)).toEqual([ORG]);
    });

    test("keeps an explicit request organization", async () => {
      const h = setup();
      fakeStream(h);
      await drain(open(h, "other-org"));
      expect(organizationsSent(h)).toEqual(["other-org"]);
    });

    test("fails locally with InvalidArgument before any fetch when no organization is set", async () => {
      const h = setup({ organizationId: undefined });
      fakeStream(h);
      const error = await drain(open(h)).then(
        () => undefined,
        (e) => e,
      );
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.InvalidArgument);
      expect(error.message).toContain("needs an organization");
      expect(h.api.fetchedUrls).toHaveLength(0);
    });
  });
});

describe("credentials", () => {
  test("never sends a request or the credential to another origin", async () => {
    const seen: string[] = [];
    const fetch = guardedFetch({
      origin: "https://ownsand.api.test",
      credential: { apiKey: API_KEY },
      organizationId: ORG,
      fetch: async (input) => {
        seen.push(String(input));
        return new Response(null);
      },
      logger: undefined,
      activity: new ActivityRegistry(),
      now: () => 0,
    });
    await expect(
      fetch("https://stun.example/delino.ownsand.v1.X/Y", {
        headers: { authorization: `Bearer ${API_KEY}` },
      }),
    ).rejects.toThrow("origin");
    expect(seen).toHaveLength(0);
    await fetch("https://ownsand.api.test/a/b", {});
    expect(seen).toEqual(["https://ownsand.api.test/a/b"]);
  });

  test("sends relay RPCs with the credential only to the configured origin", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* (req) {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).exec({ command: ["/bin/true"] });
    const result = await h.clock.run(handle.wait());
    expect(result.kind).toBe(ExecResultKind.Exited);

    const execCalls = h.api.callsOf("Exec");
    expect(execCalls).toHaveLength(1);
    expect(execCalls[0]!.headers.get("authorization")).toBe(
      `Bearer ${API_KEY}`,
    );
    const execUrls = h.api.fetchedUrls.filter((u) =>
      new URL(u).pathname.endsWith("/Exec"),
    );
    expect(execUrls).toHaveLength(1);
    expect(new URL(execUrls[0]!).origin).toBe(new URL(h.api.origin).origin);
    expect(
      h.api.fetchedUrls.every(
        (u) => new URL(u).origin === new URL(h.api.origin).origin,
      ),
    ).toBe(true);
  });

  test("redacts the credential from errors", async () => {
    const h = setup();
    h.api.unary(SandboxService, "GetSandbox", () => {
      throw new ConnectError(
        `bad key ${API_KEY} rejected`,
        Code.Unauthenticated,
      );
    });
    const error = await h.client
      .sandbox(SANDBOX)
      .get()
      .catch((e) => e);
    expect(error.code).toBe(Code.Unauthenticated);
    expect(error.message).not.toContain(API_KEY);
    expect(error.message).toContain("[redacted]");
    expect(String(error.stack)).not.toContain(API_KEY);
  });

  test("redacts the per-request access token from concurrent unary errors", async () => {
    const h = setup();
    let n = 0;
    const client = new OwnSandClient({
      origin: h.api.origin,
      fetch: h.api.fetch,
      clock: h.clock,
      env: {},
      organizationId: ORG,
      accessToken: async () => `sentinel-token-${"ABC"[n++ % 3]}`,
    });
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => {
      throw new ConnectError(
        `rejected ${call.headers.get("authorization")}`,
        Code.Unauthenticated,
      );
    });
    const errors = await Promise.all(
      [0, 1, 2].map(() =>
        client
          .sandbox(SANDBOX)
          .get()
          .catch((e) => e),
      ),
    );
    for (const error of errors) {
      expect(error.message).toContain("[redacted]");
      for (const t of ["A", "B", "C"]) {
        expect(error.message).not.toContain(`sentinel-token-${t}`);
        expect(String(error.stack)).not.toContain(`sentinel-token-${t}`);
      }
    }
  });

  test("redacts the per-request access token from streaming errors", async () => {
    const h = setup();
    const transport = createApiTransport({
      origin: h.api.origin,
      credential: { accessToken: async () => "sentinel-token-stream" },
      organizationId: ORG,
      fetch: h.api.fetch,
      logger: undefined,
      activity: new ActivityRegistry(),
      now: () => 0,
    });
    h.api.stream(RelayService, "Exec", (_req, call) => {
      throw new ConnectError(
        `rejected ${call.headers.get("authorization")}`,
        Code.Unauthenticated,
      );
    });
    const res = await transport.stream(
      RelayService.method.exec,
      undefined,
      undefined,
      undefined,
      (async function* () {
        yield create(RelayService.method.exec.input, {});
      })(),
    );
    const error = await (async () => {
      try {
        for await (const _ of res.message) {
          // Drain until the stream error.
        }
      } catch (e) {
        return e as Error;
      }
      return undefined;
    })();
    expect(error).toBeInstanceOf(ConnectError);
    expect(error!.message).toContain("[redacted]");
    expect(error!.message).not.toContain("sentinel-token-stream");
  });

  describe("an access token callback", () => {
    function transportOf(accessToken: () => string | Promise<string>) {
      const h = setup();
      h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
      const transport = createApiTransport({
        origin: h.api.origin,
        credential: { accessToken },
        organizationId: ORG,
        fetch: h.api.fetch,
        logger: undefined,
        activity: new ActivityRegistry(),
        now: () => 0,
      });
      const call = (signal?: AbortSignal) =>
        transport.unary(
          SandboxService.method.getSandbox,
          signal,
          undefined,
          undefined,
          { sandboxId: SANDBOX },
        );
      return { h, call };
    }

    test("ends a pending call with the abort reason, starts no request and leaves no abort listener", async () => {
      let resolveToken!: (token: string) => void;
      const { h, call } = transportOf(
        () => new Promise<string>((r) => (resolveToken = r)),
      );
      const abort = new AbortController();
      const before = getEventListeners(abort.signal, "abort").length;
      const reason = new Error("caller gave up");
      const outcome = call(abort.signal).catch((e) => e);
      await h.clock.settle();
      abort.abort(reason);
      // Connect wraps the abort reason of a unary call in a canceled error.
      const error = await outcome;
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.Canceled);
      expect(error.cause).toBe(reason);
      resolveToken("late-token");
      await h.clock.settle();
      expect(h.api.calls).toHaveLength(0);
      expect(getEventListeners(abort.signal, "abort")).toHaveLength(before);
    });

    test("does not call the callback for a signal that is already aborted", async () => {
      let calls = 0;
      const { h, call } = transportOf(() => {
        calls++;
        return "token";
      });
      const abort = new AbortController();
      const reason = new Error("already aborted");
      abort.abort(reason);
      const error = await call(abort.signal).catch((e) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.Canceled);
      expect(error.cause).toBe(reason);
      expect(calls).toBe(0);
      expect(h.api.calls).toHaveLength(0);
    });

    test("leaves no abort listener after a callback that resolves or rejects", async () => {
      const abort = new AbortController();
      const before = getEventListeners(abort.signal, "abort").length;
      const ok = transportOf(async () => "token");
      await ok.call(abort.signal);
      expect(ok.h.api.calls).toHaveLength(1);
      expect(ok.h.api.calls[0]!.headers.get("authorization")).toBe(
        "Bearer token",
      );
      const failing = transportOf(() => {
        throw new Error("token source failed");
      });
      const error = await failing.call(abort.signal).catch((e) => e);
      expect(error.message).toContain("token source failed");
      expect(failing.h.api.calls).toHaveLength(0);
      expect(getEventListeners(abort.signal, "abort")).toHaveLength(before);
    });

    test("does not raise an unhandled rejection when the callback rejects after the abort", async () => {
      let rejectToken!: (error: Error) => void;
      const { h, call } = transportOf(
        () => new Promise<string>((_, r) => (rejectToken = r)),
      );
      const unhandled: unknown[] = [];
      const onUnhandled = (e: unknown) => unhandled.push(e);
      process.on("unhandledRejection", onUnhandled);
      try {
        const abort = new AbortController();
        const outcome = call(abort.signal).catch((e) => e);
        await h.clock.settle();
        abort.abort(new Error("caller gave up"));
        await outcome;
        rejectToken(new Error("token source failed late"));
        await h.clock.settle();
      } finally {
        process.off("unhandledRejection", onUnhandled);
      }
      expect(unhandled).toEqual([]);
    });
  });

  test("returns an error without the secret unchanged", async () => {
    const h = setup();
    const original = new ConnectError("plain failure", Code.Internal);
    h.api.unary(SandboxService, "GetSandbox", () => {
      throw original;
    });
    const error = await h.client
      .sandbox(SANDBOX)
      .get()
      .catch((e) => e);
    expect(error.message).toContain("plain failure");
    expect(error.code).toBe(Code.Internal);
  });

  test("the logger receives RPC names and codes but no credential or content", async () => {
    const records: LogRecord[] = [];
    const h = setup({ logger: (r) => records.push(r) });
    h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
    await h.client.sandbox(SANDBOX).get();
    expect(records).toEqual([
      expect.objectContaining({
        rpc: "delino.ownsand.v1.SandboxService/GetSandbox",
        code: "ok",
      }),
    ]);
    expect(JSON.stringify(records)).not.toContain(API_KEY);
  });

  describe("a logger that returns a promise", () => {
    // Runs the body with an unhandledRejection listener and returns what it
    // saw. Node reports an unhandled rejection after a macrotask turn, so wait
    // for one before the listener is removed.
    async function collectUnhandled(body: () => Promise<void>) {
      const unhandled: unknown[] = [];
      const onUnhandled = (e: unknown) => unhandled.push(e);
      process.on("unhandledRejection", onUnhandled);
      try {
        await body();
        await new Promise((resolve) => setImmediate(resolve));
        await new Promise((resolve) => setImmediate(resolve));
      } finally {
        process.off("unhandledRejection", onUnhandled);
      }
      return unhandled;
    }

    const rejecting: Array<[string, (r: LogRecord) => unknown]> = [
      [
        "an async function that throws",
        async () => {
          throw new Error("log sink failed");
        },
      ],
      ["a rejected promise", () => Promise.reject(new Error("log sink down"))],
      [
        "a thenable that rejects",
        () => ({
          then: (_: unknown, onRejected: (e: unknown) => void) =>
            onRejected(new Error("log sink down")),
        }),
      ],
    ];

    for (const [name, logger] of rejecting) {
      test(`keeps a unary result and raises no unhandled rejection for ${name}`, async () => {
        const h = setup({ logger: logger as (r: LogRecord) => void });
        h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
        const unhandled = await collectUnhandled(async () => {
          const res = await h.client.sandbox(SANDBOX).get();
          expect(res).toBeDefined();
        });
        expect(unhandled).toEqual([]);
      });

      test(`keeps a unary error and raises no unhandled rejection for ${name}`, async () => {
        const h = setup({ logger: logger as (r: LogRecord) => void });
        h.api.unary(SandboxService, "GetSandbox", () => {
          throw new ConnectError("plain failure", Code.Internal);
        });
        const unhandled = await collectUnhandled(async () => {
          const error = await h.client
            .sandbox(SANDBOX)
            .get()
            .catch((e) => e);
          expect(error.code).toBe(Code.Internal);
        });
        expect(unhandled).toEqual([]);
      });

      test(`keeps a stream result and raises no unhandled rejection for ${name}`, async () => {
        const h = setup();
        const transport = createApiTransport({
          origin: h.api.origin,
          credential: { apiKey: API_KEY },
          organizationId: ORG,
          fetch: h.api.fetch,
          logger: logger as (r: LogRecord) => void,
          activity: new ActivityRegistry(),
          now: () => 0,
        });
        h.api.stream(RelayService, "Exec", function* () {
          yield execMsg(ev.accepted());
          yield execMsg(ev.exited(0));
        });
        const unhandled = await collectUnhandled(async () => {
          const res = await transport.stream(
            RelayService.method.exec,
            undefined,
            undefined,
            undefined,
            (async function* () {
              yield create(RelayService.method.exec.input, {});
            })(),
          );
          const seen: unknown[] = [];
          for await (const m of res.message) {
            seen.push(m);
          }
          expect(seen).toHaveLength(2);
        });
        expect(unhandled).toEqual([]);
      });
    }

    test("does not wait for a logger promise that never settles", async () => {
      const records: LogRecord[] = [];
      const h = setup({
        logger: (r) => {
          records.push(r);
          return new Promise<void>(() => {}) as unknown as void;
        },
      });
      h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
      await h.client.sandbox(SANDBOX).get();
      expect(records).toEqual([
        expect.objectContaining({ rpc: expect.stringContaining("GetSandbox") }),
      ]);
    });

    test("still isolates a synchronous throw and keeps the record format", async () => {
      const records: LogRecord[] = [];
      const h = setup({
        logger: (r) => {
          records.push(r);
          throw new Error("sync logger failure");
        },
      });
      h.api.unary(SandboxService, "GetSandbox", () => ({ sandbox: {} }));
      await h.client.sandbox(SANDBOX).get();
      expect(records).toEqual([
        expect.objectContaining({
          rpc: "delino.ownsand.v1.SandboxService/GetSandbox",
          code: "ok",
        }),
      ]);
    });
  });

  describe("streaming RPC outcomes", () => {
    const EXEC = "delino.ownsand.v1.RelayService/Exec";

    function streamHarness() {
      const records: LogRecord[] = [];
      const clock = { now: 0 };
      const h = setup();
      const transport = createApiTransport({
        origin: h.api.origin,
        credential: { apiKey: API_KEY },
        organizationId: ORG,
        fetch: h.api.fetch,
        logger: (r) => records.push(r),
        activity: new ActivityRegistry(),
        now: () => clock.now,
      });
      const open = async (signal?: AbortSignal) =>
        transport.stream(
          RelayService.method.exec,
          signal,
          undefined,
          undefined,
          (async function* () {
            yield create(RelayService.method.exec.input, {});
          })(),
        );
      return { h, records, open, clock };
    }

    test("logs nothing until the stream ends and then logs ok once", async () => {
      const { h, records, open, clock } = streamHarness();
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.exited(0));
      });
      clock.now = 1_000;
      const res = await open();
      expect(records).toEqual([]);
      clock.now = 1_250;
      const seen: unknown[] = [];
      for await (const m of res.message) {
        seen.push(m);
      }
      expect(seen).toHaveLength(2);
      expect(records).toEqual([
        { event: "rpc", rpc: EXEC, durationMs: 250, code: "ok" },
      ]);
      expect(JSON.stringify(records)).not.toContain(API_KEY);
    });

    test("logs the Connect code of the error that ends the stream", async () => {
      const { h, records, open, clock } = streamHarness();
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        throw new ConnectError(`rejected ${API_KEY}`, Code.Unavailable);
      });
      const res = await open();
      clock.now = 400;
      const error = await (async () => {
        try {
          for await (const _ of res.message) {
            // Drain until the stream error.
          }
        } catch (e) {
          return e as Error;
        }
        return undefined;
      })();
      expect(error).toBeInstanceOf(ConnectError);
      expect(records).toEqual([
        expect.objectContaining({ rpc: EXEC, code: "Unavailable" }),
      ]);
      expect(records[0]!.durationMs).toBe(400);
      expect(JSON.stringify(records)).not.toContain(API_KEY);
    });

    test("logs unknown when opening a stream fails with an error that is not a Connect error", async () => {
      const { records } = streamHarness();
      const failing = createApiTransport({
        origin: "https://ownsand.api.test",
        credential: { apiKey: API_KEY },
        organizationId: ORG,
        fetch: async () => {
          throw new TypeError("network down");
        },
        logger: (r) => records.push(r),
        activity: new ActivityRegistry(),
        now: () => 0,
      });
      await failing
        .stream(
          RelayService.method.exec,
          undefined,
          undefined,
          undefined,
          (async function* () {
            yield create(RelayService.method.exec.input, {});
          })(),
        )
        .catch(() => undefined);
      expect(records).toHaveLength(1);
      expect(records[0]!.rpc).toBe(EXEC);
      expect(records[0]!.code).toBe("unknown");
    });

    test("logs Canceled once when the consumer abandons the stream", async () => {
      const { h, records, open } = streamHarness();
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.exited(0));
      });
      const res = await open();
      for await (const _ of res.message) {
        break;
      }
      expect(records).toEqual([
        expect.objectContaining({ rpc: EXEC, code: "Canceled" }),
      ]);
      // A second return of the iterator must not log again.
      await res.message[Symbol.asyncIterator]().return?.();
      expect(records).toHaveLength(1);
    });

    test("logs the cancellation code when the caller aborts the stream", async () => {
      const { h, records, open } = streamHarness();
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        yield new Hang();
      });
      const controller = new AbortController();
      const res = await open(controller.signal);
      const iterator = res.message[Symbol.asyncIterator]();
      await iterator.next();
      controller.abort();
      await iterator.next().catch(() => undefined);
      expect(records).toHaveLength(1);
      expect(records[0]).toEqual(
        expect.objectContaining({ rpc: EXEC, code: "Canceled" }),
      );
    });

    test("logs a failure that happens before the stream opens once", async () => {
      const { h, records, open } = streamHarness();
      h.api.stream(RelayService, "Exec", () => {
        throw new ConnectError("denied", Code.PermissionDenied);
      });
      const res = await open();
      await (async () => {
        for await (const _ of res.message) {
          // Drain until the stream error.
        }
      })().catch(() => undefined);
      expect(records).toEqual([
        expect.objectContaining({ rpc: EXEC, code: "PermissionDenied" }),
      ]);
    });
  });
});

describe("createSandbox", () => {
  test.each([
    ["a missing idle TTL", { idleTtl: undefined }, "idle_ttl"],
    ["a missing max TTL", { maxTtl: undefined }, "max_ttl"],
    ["a zero idle TTL", { idleTtl: 0 }, "idle_ttl"],
    ["an idle TTL below one nanosecond", { idleTtl: 1e-7 }, "idle_ttl"],
    [
      "a max TTL below one nanosecond",
      { maxTtl: 1e-7, idleTtl: 1e-6 },
      "max_ttl",
    ],
    ["an idle TTL above the max TTL", { idleTtl: 3 * HOUR }, "idle_ttl"],
    ["a max TTL above 7 days", { maxTtl: 7 * 24 * HOUR + 1 }, "max_ttl"],
    [
      "zero vCPUs",
      { resources: { vcpuCount: 0, memoryMib: 1, diskGib: 1 } },
      "resources.vcpu_count",
    ],
  ])("rejects %s before the request", async (_name, override, field) => {
    const h = setup();
    const error = await h.client
      .createSandbox({ ...validCreate, ...override } as never)
      .catch((e) => e);
    expect(error).toBeInstanceOf(OwnSandValidationError);
    expect(error.field).toBe(field);
    expect(h.api.calls).toHaveLength(0);
  });

  test.each([
    ["a label key", { "a\0b": "v" }, "keys must not contain a NUL byte"],
    ["a label value", { k: "a\0b" }, "values must not contain a NUL byte"],
    [
      "a NUL key and a NUL value",
      { "a\0b": "c\0d" },
      "keys must not contain a NUL byte",
    ],
  ])(
    "rejects a NUL byte in %s before the request",
    async (_n, labels, rule) => {
      const h = setup();
      const error = await h.client
        .createSandbox({ ...validCreate, labels })
        .catch((e) => e);
      expect(error).toBeInstanceOf(OwnSandValidationError);
      expect(error.field).toBe("labels");
      expect(error.message).toContain(rule);
      // The message names the rule and never the label.
      expect(error.message).not.toContain("a\0b");
      expect(error.message).not.toContain("c\0d");
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test("accepts other control characters in labels", async () => {
    const h = setup();
    h.api.unary(SandboxService, "CreateSandbox", () => ({
      sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
    }));
    await h.client.createSandbox({
      ...validCreate,
      labels: { "k\u0001": "a\u0001b\n" },
    });
    const req = h.api.callsOf("CreateSandbox")[0]!.request as any;
    expect(req.labels).toEqual({ "k\u0001": "a\u0001b\n" });
  });

  const MAX_INT64 = 2n ** 63n - 1n;
  const goodResources = { vcpuCount: 2, memoryMib: 2048, diskGib: 10 };
  test.each([
    ["a fractional vCPU count", { vcpuCount: 1.5 }, "resources.vcpu_count"],
    ["a NaN vCPU count", { vcpuCount: NaN }, "resources.vcpu_count"],
    ["an infinite vCPU count", { vcpuCount: Infinity }, "resources.vcpu_count"],
    ["a vCPU count of 2**31", { vcpuCount: 2 ** 31 }, "resources.vcpu_count"],
    ["a negative vCPU count", { vcpuCount: -1 }, "resources.vcpu_count"],
    ["a fractional memory size", { memoryMib: 512.5 }, "resources.memory_mib"],
    ["a NaN memory size", { memoryMib: NaN }, "resources.memory_mib"],
    ["a zero memory size", { memoryMib: 0 }, "resources.memory_mib"],
    ["a negative memory size", { memoryMib: -1 }, "resources.memory_mib"],
    ["a zero bigint memory size", { memoryMib: 0n }, "resources.memory_mib"],
    [
      "a memory size of 2**63 as a number",
      { memoryMib: 2 ** 63 },
      "resources.memory_mib",
    ],
    [
      "a memory size of 2**53 as a number",
      { memoryMib: 2 ** 53 },
      "resources.memory_mib",
    ],
    [
      "a memory size of 2n**63n",
      { memoryMib: 2n ** 63n },
      "resources.memory_mib",
    ],
    ["a fractional disk size", { diskGib: 0.5 }, "resources.disk_gib"],
    ["a NaN disk size", { diskGib: NaN }, "resources.disk_gib"],
    ["a zero disk size", { diskGib: 0 }, "resources.disk_gib"],
    ["a negative disk size", { diskGib: -5 }, "resources.disk_gib"],
    ["a disk size of 2**63", { diskGib: 2 ** 63 }, "resources.disk_gib"],
    ["a disk size of 2n**63n", { diskGib: 2n ** 63n }, "resources.disk_gib"],
  ])("rejects %s before the request", async (_name, resources, field) => {
    const h = setup();
    const error = await h.client
      .createSandbox({
        ...validCreate,
        resources: { ...goodResources, ...resources },
      } as never)
      .catch((e) => e);
    expect(error).toBeInstanceOf(OwnSandValidationError);
    expect(error.field).toBe(field);
    expect(h.api.calls).toHaveLength(0);
  });

  test("keeps the zero and negative resource messages", async () => {
    const h = setup();
    const messages = await Promise.all(
      [{ vcpuCount: 0 }, { memoryMib: -1 }, { diskGib: 0n }].map((resources) =>
        h.client
          .createSandbox({
            ...validCreate,
            resources: { ...goodResources, ...resources },
          } as never)
          .catch((e) => e.message),
      ),
    );
    for (const message of messages) {
      expect(message).toContain("must be greater than zero");
    }
  });

  test("never puts the rejected resource value in the error", async () => {
    const h = setup();
    const error = await h.client
      .createSandbox({
        ...validCreate,
        resources: { ...goodResources, memoryMib: 123456.789 },
      } as never)
      .catch((e) => e);
    expect(error.message).not.toContain("123456");
  });

  type Resources = CreateSandboxInput["resources"];
  const acceptedResources: [
    name: string,
    resources: Partial<Resources>,
    want: { vcpuCount: number; memoryMib: bigint; diskGib: bigint },
  ][] = [
    [
      "the largest vCPU count",
      { vcpuCount: 2 ** 31 - 1 },
      { vcpuCount: 2 ** 31 - 1, memoryMib: 2048n, diskGib: 10n },
    ],
    [
      "the largest safe integer sizes",
      {
        memoryMib: Number.MAX_SAFE_INTEGER,
        diskGib: Number.MAX_SAFE_INTEGER,
      },
      {
        vcpuCount: 2,
        memoryMib: BigInt(Number.MAX_SAFE_INTEGER),
        diskGib: BigInt(Number.MAX_SAFE_INTEGER),
      },
    ],
    [
      "bigint sizes",
      { memoryMib: 4096n, diskGib: 20n },
      { vcpuCount: 2, memoryMib: 4096n, diskGib: 20n },
    ],
    [
      "the largest bigint sizes",
      { memoryMib: MAX_INT64, diskGib: MAX_INT64 },
      { vcpuCount: 2, memoryMib: MAX_INT64, diskGib: MAX_INT64 },
    ],
    [
      "a bigint size next to a number size",
      { memoryMib: 4096n, diskGib: 20 },
      { vcpuCount: 2, memoryMib: 4096n, diskGib: 20n },
    ],
  ];
  test.each(acceptedResources)("accepts %s", async (_name, resources, want) => {
    const h = setup();
    h.api.unary(SandboxService, "CreateSandbox", () => ({
      sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
    }));
    await h.client.createSandbox({
      ...validCreate,
      resources: { ...goodResources, ...resources },
    });
    const req = h.api.callsOf("CreateSandbox")[0]!.request as any;
    expect(req.resources.vcpuCount).toBe(want.vcpuCount);
    expect(req.resources.memoryMib).toBe(want.memoryMib);
    expect(req.resources.diskGib).toBe(want.diskGib);
  });

  test("types vcpuCount as a number only", () => {
    const resources: Resources = {
      // @ts-expect-error vcpuCount is an int32 and takes no bigint.
      vcpuCount: 1n,
      memoryMib: 1n,
      diskGib: 1n,
    };
    expect(resources.memoryMib).toBe(1n);
  });

  test("accepts a max TTL of exactly 7 days", async () => {
    const h = setup();
    h.api.unary(SandboxService, "CreateSandbox", () => ({
      sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
    }));
    const sandbox = await h.client.createSandbox({
      ...validCreate,
      maxTtl: 7 * 24 * HOUR,
    });
    expect(sandbox.sandboxId).toBe(SANDBOX);
    const req = h.api.callsOf("CreateSandbox")[0]!.request as any;
    expect(req.maxTtl.seconds).toBe(604800n);
  });

  test.each([
    [0.5, 0n, 500_000],
    [1.5, 0n, 1_500_000],
    [1000.25, 1n, 250_000],
    [1.0000004, 0n, 1_000_000 + 0],
  ])(
    "keeps the fractional part of a %s ms TTL on the wire",
    async (ms, seconds, nanos) => {
      const h = setup();
      h.api.unary(SandboxService, "CreateSandbox", () => ({
        sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
      }));
      await h.client.createSandbox({
        ...validCreate,
        idleTtl: ms,
        maxTtl: HOUR,
      });
      const req = h.api.callsOf("CreateSandbox")[0]!.request as any;
      expect(req.idleTtl.seconds).toBe(seconds);
      expect(req.idleTtl.nanos).toBe(nanos);
    },
  );

  test("compares the TTL ordering on the precise values", async () => {
    const h = setup();
    const error = await h.client
      .createSandbox({ ...validCreate, idleTtl: 1.5, maxTtl: 1.4 })
      .catch((e) => e);
    expect(error).toBeInstanceOf(OwnSandValidationError);
    expect(error.field).toBe("idle_ttl");
    expect(h.api.calls).toHaveLength(0);
  });

  test("reuses one idempotency key for every retry of a create", async () => {
    const h = setup();
    h.api.unary(SandboxService, "CreateSandbox", (_req, call) => {
      if (call.index === 0) throw new LostResponse();
      if (call.index === 1) throw new ConnectError("restart", Code.Unavailable);
      return { sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING } };
    });
    await h.clock.run(h.client.createSandbox(validCreate));
    const keys = h.api
      .callsOf("CreateSandbox")
      .map((c) => (c.request as any).idempotencyKey);
    expect(keys).toHaveLength(3);
    expect(new Set(keys).size).toBe(1);
    expect(isUuidV7(keys[0])).toBe(true);
    // A second logical create has a new key.
    await h.clock.run(h.client.createSandbox(validCreate));
    const all = h.api
      .callsOf("CreateSandbox")
      .map((c) => (c.request as any).idempotencyKey);
    expect(new Set(all).size).toBe(2);
  });

  describe("a forced PEER_TO_PEER transport without WebRTC", () => {
    const answer = () => ({
      sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
    });

    test.each([
      ["webrtc: false", { webrtc: false as const }],
      // The Node.js test runtime has no RTCPeerConnection of its own.
      ["no runtime RTCPeerConnection", { webrtc: undefined }],
    ])(
      "rejects createSandbox with a config error before CreateSandbox (%s)",
      async (_name, webrtc) => {
        const h = setup({
          transport: DataPathTransport.PEER_TO_PEER,
          ...webrtc,
        });
        h.api.unary(SandboxService, "CreateSandbox", answer);
        const error = await h.clock
          .run(h.client.createSandbox(validCreate))
          .catch((e) => e);
        expect(error).toBeInstanceOf(OwnSandConfigError);
        expect(error.message).toBe(
          "PEER_TO_PEER transport needs a WebRTC implementation",
        );
        expect(h.api.callsOf("CreateSandbox")).toHaveLength(0);
        expect(h.api.calls).toHaveLength(0);
      },
    );

    test("still throws from sandbox(id)", () => {
      const h = setup({ transport: DataPathTransport.PEER_TO_PEER });
      expect(() => h.client.sandbox(SANDBOX)).toThrow(OwnSandConfigError);
      expect(h.api.calls).toHaveLength(0);
    });

    test.each([
      ["the default transport", undefined],
      ["the RELAY transport", DataPathTransport.RELAY],
    ])("still creates a sandbox with %s", async (_name, transport) => {
      const h = setup({ transport });
      h.api.unary(SandboxService, "CreateSandbox", answer);
      const sandbox = await h.clock.run(h.client.createSandbox(validCreate));
      expect(sandbox.sandboxId).toBe(SANDBOX);
      expect(sandbox.transport).toBe(DataPathTransport.RELAY);
      expect(h.api.callsOf("CreateSandbox")).toHaveLength(1);
    });
  });

  describe("the 60-second retry window", () => {
    const created = {
      sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
    };
    const never = () => new Promise<never>(() => {});

    /** Registers a CreateSandbox fake and records the virtual time of each call. */
    function recorded(
      h: ReturnType<typeof setup>,
      handler: (call: { index: number }) => unknown,
    ): number[] {
      const times: number[] = [];
      h.api.unary(SandboxService, "CreateSandbox", (_req, call) => {
        times.push(h.clock.now());
        return handler(call) as never;
      });
      return times;
    }
    const gaps = (times: number[]) =>
      times.slice(1).map((t, i) => t - times[i]!);
    const keysOf = (h: ReturnType<typeof setup>) =>
      new Set(
        h.api
          .callsOf("CreateSandbox")
          .map((c) => (c.request as any).idempotencyKey),
      );

    test("retries more than four lost responses with one key and the back-off schedule", async () => {
      const h = setup();
      const times = recorded(h, (call) => {
        if (call.index >= 6) return created;
        if (call.index % 2 === 0) throw new LostResponse();
        throw new ConnectError("restart", Code.Unavailable);
      });
      const sandbox = await h.clock.run(h.client.createSandbox(validCreate));
      expect(sandbox.sandboxId).toBe(SANDBOX);
      expect(times).toHaveLength(7);
      expect(gaps(times)).toEqual([500, 1000, 2000, 4000, 5000, 5000]);
      expect(keysOf(h).size).toBe(1);
    });

    test("retries a deadline_exceeded with the same key", async () => {
      const h = setup();
      recorded(h, (call) => {
        if (call.index === 0) {
          throw new ConnectError("too slow", Code.DeadlineExceeded);
        }
        return created;
      });
      await h.clock.run(h.client.createSandbox(validCreate));
      expect(h.api.callsOf("CreateSandbox")).toHaveLength(2);
      expect(keysOf(h).size).toBe(1);
    });

    test("waits at least the retry_after of a rate limit", async () => {
      const h = setup();
      const times = recorded(h, (call) => {
        if (call.index === 0) {
          throw ownsandError(Code.ResourceExhausted, "rate limited", {
            retryAfterMs: 10_000,
          });
        }
        return created;
      });
      await h.clock.run(h.client.createSandbox(validCreate));
      expect(gaps(times)).toEqual([10_000]);
    });

    test("returns the failure at once when the retry_after is longer than the window", async () => {
      const h = setup();
      const times = recorded(h, () => {
        throw ownsandError(Code.ResourceExhausted, "rate limited", {
          retryAfterMs: 120_000,
        });
      });
      const error = await h.clock
        .run(h.client.createSandbox(validCreate))
        .catch((e) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.ResourceExhausted);
      expect(times).toEqual([0]);
    });

    test("ends with the last failure within 60 seconds when every attempt is unavailable", async () => {
      const h = setup();
      const times = recorded(h, (call) => {
        throw new ConnectError(`down ${call.index}`, Code.Unavailable);
      });
      const error = await h.clock
        .run(h.client.createSandbox(validCreate))
        .catch((e) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.Unavailable);
      expect(error.rawMessage).toBe(`down ${times.length - 1}`);
      expect(h.clock.now()).toBeLessThanOrEqual(60_000);
      expect(times.length).toBeGreaterThan(4);
      expect(keysOf(h).size).toBe(1);
    });

    test("cancels a retry that never answers at the window end and returns the last failure", async () => {
      const h = setup();
      const times = recorded(h, (call) => {
        if (call.index === 0) {
          throw new ConnectError("first failure", Code.Unavailable);
        }
        return never();
      });
      const error = await h.clock
        .run(h.client.createSandbox(validCreate))
        .catch((e) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect(error.code).toBe(Code.Unavailable);
      expect(error.rawMessage).toBe("first failure");
      // The window starts with the first failure, at virtual time 0.
      expect(h.clock.now()).toBe(60_000);
      expect(times).toEqual([0, 500]);
    });

    test("does not set a Connect timeout on the retry", async () => {
      const h = setup();
      recorded(h, (call) => {
        if (call.index === 0) throw new LostResponse();
        return created;
      });
      await h.clock.run(h.client.createSandbox(validCreate));
      for (const call of h.api.callsOf("CreateSandbox")) {
        expect(call.headers.get("connect-timeout-ms")).toBeNull();
      }
    });

    test("ends a billing resource_exhausted after one call", async () => {
      const h = setup();
      const times = recorded(h, () => {
        throw ownsandError(Code.ResourceExhausted, "suspended", {
          billingReason: BillingReason.SUSPENDED,
        });
      });
      const error = await h.clock
        .run(h.client.createSandbox(validCreate))
        .catch((e) => e);
      expect(error.code).toBe(Code.ResourceExhausted);
      expect(times).toHaveLength(1);
    });

    test("a caller abort during the back-off rejects with the caller's reason and sends no further call", async () => {
      const h = setup();
      const times = recorded(h, () => {
        throw new ConnectError("down", Code.Unavailable);
      });
      const abort = new AbortController();
      const reason = new Error("caller gave up");
      const outcome = h.client
        .createSandbox({ ...validCreate, signal: abort.signal })
        .catch((e) => e);
      await h.clock.advance(700);
      expect(times).toEqual([0, 500]);
      abort.abort(reason);
      await h.clock.advance(60_000);
      expect(await outcome).toBe(reason);
      expect(times).toEqual([0, 500]);
    });

    test("a caller abort during a stalled retry rejects with the caller's reason", async () => {
      const h = setup();
      const times = recorded(h, (call) => {
        if (call.index === 0) throw new ConnectError("down", Code.Unavailable);
        return never();
      });
      const abort = new AbortController();
      const reason = new Error("caller gave up");
      const outcome = h.client
        .createSandbox({ ...validCreate, signal: abort.signal })
        .catch((e) => e);
      await h.clock.advance(10_000);
      abort.abort(reason);
      await h.clock.advance(120_000);
      expect(await outcome).toBe(reason);
      expect(times).toEqual([0, 500]);
    });

    test("leaves no listener on the caller's signal and no timer after the create", async () => {
      const h = setup();
      // The retry-window timer only aborts a private controller, so advancing
      // the clock cannot show a leaked timer. Count the sleeps that have not
      // settled instead: the window timer settles only when its controller is
      // aborted or its time is up.
      let pending = 0;
      const sleep = h.clock.sleep.bind(h.clock);
      h.clock.sleep = (ms, signal) => {
        pending++;
        return sleep(ms, signal).finally(() => {
          pending--;
        });
      };
      recorded(h, (call) => {
        if (call.index === 0) throw new LostResponse();
        return created;
      });
      const abort = new AbortController();
      const before = getEventListeners(abort.signal, "abort").length;
      await h.clock.run(
        h.client.createSandbox({ ...validCreate, signal: abort.signal }),
      );
      await h.clock.settle();
      expect(getEventListeners(abort.signal, "abort")).toHaveLength(before);
      expect(pending).toBe(0);
    });
  });

  test("waits until the sandbox is stable", async () => {
    const h = setup();
    h.api.unary(SandboxService, "CreateSandbox", () => ({
      sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING },
    }));
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => ({
      sandbox: {
        sandboxId: SANDBOX,
        state: call.index < 2 ? SandboxState.CREATING : SandboxState.RUNNING,
      },
    }));
    const sandbox = await h.clock.run(h.client.createSandbox(validCreate));
    const stable = await h.clock.run(sandbox.waitUntilStable());
    expect(stable.state).toBe(SandboxState.RUNNING);
    expect(sandbox.info?.state).toBe(SandboxState.RUNNING);
  });
});

describe("sandbox ID canonicalization", () => {
  const UPPER = SANDBOX.toUpperCase();

  test("stores a UUID v7 in either hex case in lowercase", () => {
    const h = setup();
    expect(UPPER).not.toBe(SANDBOX);
    expect(h.client.sandbox(UPPER).sandboxId).toBe(SANDBOX);
    expect(h.client.sandbox(SANDBOX).sandboxId).toBe(SANDBOX);
    const mixed = `${SANDBOX.slice(0, 18)}${SANDBOX.slice(18).toUpperCase()}`;
    expect(h.client.sandbox(mixed).sandboxId).toBe(SANDBOX);
  });

  test("keeps a value that is not a UUID v7 unchanged", () => {
    const h = setup();
    for (const value of [
      "My-Sandbox",
      "",
      // A UUID v4 is not a UUID v7.
      "0192F4B6-7C3A-4DEF-8ABC-0123456789AB",
      // A trailing character is not part of a UUID.
      `${UPPER}\n`,
    ]) {
      expect(h.client.sandbox(value).sandboxId).toBe(value);
    }
  });

  test("sends the lowercase ID in unary requests, relay RPCs and the request digest", async () => {
    const h = setup();
    h.api.unary(SandboxService, "GetSandbox", (req) => ({
      sandbox: { sandboxId: req.sandboxId, state: SandboxState.RUNNING },
    }));
    h.api.stream(RelayService, "Exec", function* (req) {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started(req.execId));
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(UPPER);
    await sandbox.get();
    const handle = sandbox.exec({ command: ["/bin/true"] });
    await h.clock.run(handle.wait());

    expect((h.api.callsOf("GetSandbox")[0]!.request as any).sandboxId).toBe(
      SANDBOX,
    );
    const auth = h.api.callsOf("AuthorizeRelay")[0]!.request as any;
    const exec = h.api.callsOf("Exec")[0]!.request as any;
    expect(auth.sandboxId).toBe(SANDBOX);
    expect(exec.sandboxId).toBe(SANDBOX);
    // The digest equals the digest of the same request with the lowercase ID.
    expect(Buffer.from(auth.requestDigest)).toEqual(
      Buffer.from(requestDigest(execBinding(exec))),
    );
    expect(Buffer.from(auth.requestDigest)).toEqual(
      Buffer.from(requestDigest(execBinding({ ...exec, sandboxId: SANDBOX }))),
    );
  });

  test("sends the lowercase ID in file transfer relay requests", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      yield file.accepted();
      yield file.chunk(0, "x");
      yield file.end(1);
    });
    h.api.unary(RelayService, "WriteFile", (req) => ({
      writeId: req.writeId,
      offset: req.offset,
      length: BigInt(req.data.length),
      finished: true,
      sizeBytes: req.finish?.sizeBytes ?? 0n,
    }));
    const sandbox = h.client.sandbox(UPPER);
    await h.clock.run(sandbox.readFileBytes("/f"));
    await h.clock.run(sandbox.writeFile("/f", "hello"));
    for (const name of ["ReadFile", "WriteFile"]) {
      expect((h.api.callsOf(name)[0]!.request as any).sandboxId).toBe(SANDBOX);
    }
    for (const call of h.api.callsOf("AuthorizeRelay")) {
      expect((call.request as any).sandboxId).toBe(SANDBOX);
    }
  });

  test("sends a value that is not a UUID v7 as given", async () => {
    const h = setup();
    h.api.unary(SandboxService, "GetSandbox", (req) => ({
      sandbox: { sandboxId: req.sandboxId, state: SandboxState.RUNNING },
    }));
    await h.client.sandbox("My-Sandbox").get();
    expect((h.api.callsOf("GetSandbox")[0]!.request as any).sandboxId).toBe(
      "My-Sandbox",
    );
  });
});
