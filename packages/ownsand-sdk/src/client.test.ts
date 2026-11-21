// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OwnSandClient } from "./client.ts";
import {
  ActivityRegistry,
  createApiTransport,
  DEFAULT_API_ORIGIN,
  guardedFetch,
  resolveApiOrigin,
} from "./connection.ts";
import { OwnSandConfigError, OwnSandValidationError } from "./errors.ts";
import { BaseImage, SandboxState } from "./gen/delino/ownsand/v1/common_pb.ts";
import { MemberService } from "./gen/delino/ownsand/v1/member_pb.ts";
import { OrganizationService } from "./gen/delino/ownsand/v1/organization_pb.ts";
import { RelayService } from "./gen/delino/ownsand/v1/relay_pb.ts";
import { SandboxService } from "./gen/delino/ownsand/v1/sandbox_pb.ts";
import { ExecResultKind } from "./operations/exec.ts";
import { type LogRecord } from "./logger.ts";
import { isUuidV7 } from "./uuidv7.ts";
import { LostResponse } from "./testing/fake-api.ts";
import {
  API_KEY,
  ev,
  execMsg,
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
});

describe("createSandbox", () => {
  test.each([
    ["a missing idle TTL", { idleTtl: undefined }, "idle_ttl"],
    ["a missing max TTL", { maxTtl: undefined }, "max_ttl"],
    ["a zero idle TTL", { idleTtl: 0 }, "idle_ttl"],
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
