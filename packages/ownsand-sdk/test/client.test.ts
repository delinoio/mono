// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createConnectRouter } from "@connectrpc/connect";
import {
  createUniversalHandlerClient,
  type UniversalClientRequest,
} from "@connectrpc/connect/protocol";
import { describe, expect, test } from "@rstest/core";

import { DEFAULT_ORIGIN, OwnSandClient, parseOrigin } from "../src/client.ts";
import {
  BaseImage,
  DataPathTransport,
} from "../src/gen/delino/ownsand/v1/common_pb.ts";
import { OrganizationService } from "../src/gen/delino/ownsand/v1/organization_pb.ts";
import {
  ACTIVITY_HEADER,
  ActivityRegistry,
  createSdkHttpClient,
} from "../src/http.ts";
import { LogEvent } from "../src/log.ts";
import { API_KEY, disconnect, ORG, SANDBOX } from "./support/fake.ts";
import { harness, ORIGIN, rejection } from "./support/harness.ts";

const createOptions = {
  source: { baseImage: BaseImage.LINUX },
  resources: { vcpuCount: 2, memoryMib: 2048, diskGib: 10 },
  idleTtl: 10 * 60_000,
  maxTtl: 60 * 60_000,
};

describe("client construction", () => {
  test("defaults to the production origin and reads OWNSAND_API_KEY and OWNSAND_ORG", () => {
    const client = new OwnSandClient({
      env: { OWNSAND_API_KEY: "osk_env", OWNSAND_ORG: ORG },
    });
    expect(client.origin).toBe(DEFAULT_ORIGIN);
    expect(client.sandbox(SANDBOX).organizationId).toBe(ORG);
    expect(client.transport).toBe(DataPathTransport.RELAY);
  });

  test("requires exactly one credential", () => {
    expect(() => new OwnSandClient({ env: {} })).toThrow(/no credential/);
    expect(
      () =>
        new OwnSandClient({ env: {}, apiKey: "osk_a", accessToken: () => "t" }),
    ).toThrow(/either/);
  });

  test("rejects a forced peer-to-peer transport", () => {
    const err = (() => {
      try {
        new OwnSandClient({
          env: {},
          apiKey: "osk_a",
          transport: DataPathTransport.PEER_TO_PEER,
        });
      } catch (e) {
        return e as ConnectError;
      }
      throw new Error("expected an error");
    })();
    expect(err.code).toBe(Code.Unimplemented);
    expect(err.message).toMatch(/RELAY/);
  });

  test("validates the origin", () => {
    expect(parseOrigin("https://example.com/")).toBe("https://example.com");
    expect(parseOrigin("http://127.0.0.1:8080")).toBe("http://127.0.0.1:8080");
    expect(() => parseOrigin("http://example.com")).toThrow(/https/);
    expect(() => parseOrigin("https://example.com/api")).toThrow(
      /without path/,
    );
    expect(() => parseOrigin("https://user:pw@example.com")).toThrow(
      /without path/,
    );
  });
});

describe("credentials and origin", () => {
  test("sends credentials only to the configured API origin", async () => {
    const seen: UniversalClientRequest[] = [];
    const http = createSdkHttpClient({
      origin: "https://ownsand.api.delino.io",
      credential: async () => API_KEY,
      activity: new ActivityRegistry(),
      inner: async (req) => {
        seen.push(req);
        return {
          status: 200,
          header: new Headers(),
          trailer: new Headers(),
          body: (async function* () {})(),
        };
      },
    });
    const err = await rejection(
      http({
        url: "https://evil.example.com/delino.ownsand.v1.SandboxService/GetSandbox",
        method: "POST",
        header: new Headers(),
      }),
    );
    expect(err.message).toMatch(/outside the configured API origin/);
    expect(seen).toHaveLength(0);

    const header = new Headers({ [ACTIVITY_HEADER]: "1" });
    await http({
      url: "https://ownsand.api.delino.io/x.Y/Z",
      method: "POST",
      header,
    });
    expect(seen).toHaveLength(1);
    expect(seen[0]!.header.get("authorization")).toBe(`Bearer ${API_KEY}`);
    // The internal activity header never leaves the process.
    expect(seen[0]!.header.has(ACTIVITY_HEADER)).toBe(false);
  });

  test("every RPC, also on the relay, carries the credential", async () => {
    const h = harness();
    await h.run(h.sandbox.exec(["true"]).wait());
    // The fake rejects any request without the API key, so success shows the header was present.
    expect(h.fake.callsOf("AuthorizeRelay")).toHaveLength(1);
    expect(h.fake.callsOf("Exec")).toHaveLength(1);
  });

  test("redacts the API key and access tokens from errors", async () => {
    const router = createConnectRouter();
    router.service(OrganizationService, {
      getOrganization: (_req, ctx) => {
        throw new ConnectError(
          `credential ${ctx.requestHeader.get("authorization")} was rejected`,
          Code.Unauthenticated,
          {
            "x-echo": ctx.requestHeader.get("authorization") ?? "",
          },
        );
      },
    });
    const httpClient = createUniversalHandlerClient(router.handlers);
    for (const credential of [
      { apiKey: "osk_super_secret_key" },
      { accessToken: () => "eyJ.secret.token" },
    ]) {
      const client = new OwnSandClient({
        origin: ORIGIN,
        env: {},
        organizationId: ORG,
        httpClient,
        ...credential,
      });
      const err = (await rejection(
        client.organizations.getOrganization({}),
      )) as ConnectError;
      expect(err.code).toBe(Code.Unauthenticated);
      expect(err.message).not.toMatch(
        /osk_super_secret_key|eyJ\.secret\.token/,
      );
      expect(err.message).toMatch(/\[REDACTED\]/);
      expect(String(err.metadata.get("x-echo"))).not.toMatch(/secret/);
      expect(err.cause).toBeUndefined();
    }
  });

  test("a revoked credential ends a relay RPC at once with unauthenticated", async () => {
    const h = harness();
    h.fake.programs.set("sleep", [{ delayMs: 10 * 60_000, exit: 0 }]);
    const run = h.sandbox.exec(["sleep"]);
    const p = run.wait();
    h.fake.hooks.stream = (_rpc, n) => {
      if (n === 0) {
        return { disconnectAfterMs: 5_000 };
      }
      // The credential was revoked while the operation was in recovery.
      return {
        reject: new ConnectError("credential revoked", Code.Unauthenticated),
      };
    };
    const err = (await h.run(rejection(p))) as ConnectError;
    expect(err.code).toBe(Code.Unauthenticated);
    expect(h.clock.now()).toBeLessThan(10_000);
  });
});

describe("service wrappers", () => {
  test("fill the default organization and accept an override", async () => {
    const h = harness();
    await h.run(h.client.organizations.getOrganization());
    await h.run(
      h.client.organizations.getOrganization({ organizationId: "other-org" }),
    );
    expect(h.fake.callsOf("GetOrganization").map((c) => c.operationId)).toEqual(
      [ORG, "other-org"],
    );
  });

  test("fail before sending when no organization is known", async () => {
    const h = harness({ organizationId: undefined });
    const err = (await h.run(
      rejection(h.client.sandboxService.listSandboxes()),
    )) as ConnectError;
    expect(err.code).toBe(Code.InvalidArgument);
    expect(err.message).toMatch(/OWNSAND_ORG/);
    expect(h.fake.callsOf("ListSandboxes")).toHaveLength(0);
    expect(() => h.client.sandbox(SANDBOX)).toThrow(/organization/);
    // RPCs without an organization still work.
    await h.run(h.client.organizations.listOrganizations());
    expect(h.fake.callsOf("ListOrganizations")).toHaveLength(1);
  });

  test("OpenSession is never called and fails with a clear error", async () => {
    const h = harness();
    const err = (await h.run(
      rejection(h.client.dataPath.openSession({ sandboxId: SANDBOX })),
    )) as ConnectError;
    expect(err.code).toBe(Code.Unimplemented);
    expect(err.message).toMatch(/relay-only/);
    await h.run(h.sandbox.exec(["true"]).wait());
    await h.run(h.sandbox.writeFile("/f", "x"));
    await h.run(h.sandbox.readFile("/f").bytes());
    expect(h.fake.openSessionCalls).toBe(0);
    expect(h.sandbox.transport).toBe(DataPathTransport.RELAY);
  });

  test("logs RPC names, sandbox IDs and codes, never credentials or content", async () => {
    const h = harness();
    h.fake.programs.set("echo secret-output", [
      { delayMs: 0, stdout: "secret-output", exit: 0 },
    ]);
    await h.run(
      h.sandbox
        .exec(["echo", "secret-output"], { env: { TOKEN: "env-secret" } })
        .wait(),
    );
    await h.run(h.sandbox.writeFile("/f", "file-secret"));
    const dump = JSON.stringify(h.logs, (_k, v: unknown) =>
      typeof v === "bigint" ? v.toString() : v,
    );
    expect(dump).not.toMatch(/osk_|secret/);
    expect(
      h.logs.some(
        (r) =>
          r.event === LogEvent.RPC &&
          r.rpc === "AuthorizeRelay" &&
          r.sandboxId === SANDBOX,
      ),
    ).toBe(true);
    expect(
      h.logs.some(
        (r) =>
          r.event === LogEvent.TRANSPORT_SELECTED &&
          r.transport === DataPathTransport.RELAY,
      ),
    ).toBe(true);
  });
});

describe("create", () => {
  test("validates TTLs and resources before any request", async () => {
    const h = harness();
    const cases: [Partial<typeof createOptions>, RegExp][] = [
      [{ idleTtl: undefined as never }, /idleTtl is required/],
      [{ maxTtl: 0 }, /maxTtl is required/],
      [{ idleTtl: 2 * 60 * 60_000 }, /not be greater than maxTtl/],
      [{ maxTtl: 7 * 24 * 60 * 60_000 + 1 }, /7 days/],
      [{ resources: { vcpuCount: 0, memoryMib: 1, diskGib: 1 } }, /vcpuCount/],
    ];
    for (const [override, message] of cases) {
      const err = (await h.run(
        rejection(h.client.create({ ...createOptions, ...override })),
      )) as ConnectError;
      expect(err.code).toBe(Code.InvalidArgument);
      expect(err.message).toMatch(message);
    }
    expect(h.fake.callsOf("CreateSandbox")).toHaveLength(0);
    // Exactly 7 days is valid.
    await h.run(
      h.client.create({ ...createOptions, maxTtl: 7 * 24 * 60 * 60_000 }),
    );
    expect(h.fake.callsOf("CreateSandbox")).toHaveLength(1);
  });

  test("reuses one idempotency key for every retry of a create", async () => {
    const h = harness();
    h.fake.hooks.createSandbox = (n) => (n < 2 ? "lose" : undefined);
    const first = await h.run(h.client.create(createOptions));
    const keys = h.fake.callsOf("CreateSandbox").map((c) => c.authorizationId);
    expect(keys).toHaveLength(3);
    expect(new Set(keys).size).toBe(1);
    expect(first.handle.sandboxId).toBe(first.sandbox.sandboxId);
    await h.run(h.client.create(createOptions));
    expect(
      new Set(h.fake.callsOf("CreateSandbox").map((c) => c.authorizationId))
        .size,
    ).toBe(2);
  });
});

export { disconnect };
