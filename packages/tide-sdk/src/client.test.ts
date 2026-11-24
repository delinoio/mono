// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { Role } from "@delino/tide-protos/v1/common_pb";
import { DevSessionService } from "@delino/tide-protos/v1/dev_session_pb";
import { OrganizationService } from "@delino/tide-protos/v1/organization_pb";
import { RunnerProfileService } from "@delino/tide-protos/v1/runner_profile_pb";
import { TideClient } from "./client.ts";
import {
  DEFAULT_API_ORIGIN,
  guardedFetch,
  resolveApiOrigin,
} from "./connection.ts";
import { connectCodeOf, TideConfigError, TideError } from "./errors.ts";
import { FAKE_ORIGIN, fakeServer } from "./testing/fake-server.ts";

const ORG = "0193a000-0000-7000-8000-000000000001";

describe("client configuration", () => {
  test("uses the production origin by default", () => {
    expect(resolveApiOrigin(undefined)).toBe(DEFAULT_API_ORIGIN);
    expect(new TideClient({ token: "test-token" }).origin).toBe(
      "https://tide.api.delino.io",
    );
  });

  test("accepts a loopback origin and rejects a path, query or credentials", () => {
    expect(resolveApiOrigin("http://127.0.0.1:18400/")).toBe(
      "http://127.0.0.1:18400",
    );
    for (const bad of [
      "https://a.test/api",
      "https://a.test?x=1",
      "https://u:p@a.test",
      "ftp://a.test",
      "not a url",
    ]) {
      expect(() => resolveApiOrigin(bad)).toThrow(TideConfigError);
    }
  });

  test.each([
    "https://api.example.test\\tenant\\..",
    "https://api.example.test\\",
    "https://api.example.test/tenant/..",
    "https:///api.example.test/tenant/..",
    "https:///api.example.test/tenant/../",
    "https:api.example.test/tenant/..",
    "https:/api.example.test/tenant/..",
    "https:////api.example.test/tenant/..",
    "https://api.example.test/./",
  ])("rejects an origin with a hidden path (%s)", (origin) => {
    const resolveOrigin = () => resolveApiOrigin(origin);
    const createClient = () => new TideClient({ origin, token: "test-token" });
    const expectedMessage = /^origin must not have a path, query or fragment$/;
    expect(resolveOrigin).toThrow(TideConfigError);
    expect(resolveOrigin).toThrow(expectedMessage);
    expect(createClient).toThrow(TideConfigError);
    expect(createClient).toThrow(expectedMessage);
  });

  test.each([
    ["", DEFAULT_API_ORIGIN],
    ["https://tide.api.delino.io", DEFAULT_API_ORIGIN],
    ["HTTPS://API.EXAMPLE.TEST", "https://api.example.test"],
    ["https://a.test/", "https://a.test"],
    [" \t https://A.TEST/ \r\n", "https://a.test"],
    ["http://[::1]:8080/", "http://[::1]:8080"],
  ])("canonicalizes a valid explicit origin (%s)", (origin, expected) => {
    expect(resolveApiOrigin(origin)).toBe(expected);
    expect(new TideClient({ origin, token: "test-token" }).origin).toBe(
      expected,
    );
  });

  test("rejects an empty token", () => {
    expect(() => new TideClient({ token: "" })).toThrow(TideConfigError);
  });

  test("creates a client for every public service", () => {
    const client = new TideClient({ token: "test-token" });
    for (const name of [
      "organizations",
      "members",
      "ownSandLink",
      "gitHub",
      "agentIntegrations",
      "runnerProfiles",
      "work",
      "apiKeys",
      "devSessions",
      "audit",
    ] as const) {
      expect(typeof client[name]).toBe("object");
    }
    expect(typeof client.organizations.createOrganization).toBe("function");
    expect(typeof client.devSessions.attachDevSessionExec).toBe("function");
  });
});

describe("credentials", () => {
  test("asks the token source before every call and sends a bearer token", async () => {
    const server = fakeServer((router) => {
      router.service(OrganizationService, {
        getOrganization: (req) => ({
          organization: {
            organizationId: req.organizationId,
            name: "Acme",
            callerRole: Role.OWNER,
          },
        }),
      });
    });
    let n = 0;
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: async () => `token-${++n}`,
    });
    const res = await client.organizations.getOrganization({
      organizationId: ORG,
    });
    await client.organizations.getOrganization({ organizationId: ORG });
    expect(res.organization?.name).toBe("Acme");
    expect(res.organization?.callerRole).toBe(Role.OWNER);
    const headers = server.calls.map((c) => c.headers.get("authorization"));
    expect(headers).toEqual(["Bearer token-1", "Bearer token-2"]);
    expect(server.calls.every((c) => c.url.startsWith(`${FAKE_ORIGIN}/`))).toBe(
      true,
    );
  });

  test("cancellation leaves no unhandled token provider rejection", () => {
    const fixture = fileURLToPath(
      new URL("./testing/token-provider-cancellation.ts", import.meta.url),
    );
    execFileSync(
      process.execPath,
      [
        "--unhandled-rejections=strict",
        "--experimental-transform-types",
        "--test",
        fixture,
      ],
      { timeout: 10_000 },
    );
  });

  test("an already aborted call skips the token source and fetch", async () => {
    const controller = new AbortController();
    controller.abort();
    const server = fakeServer(() => {});
    let providerCalls = 0;
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: () => {
        providerCalls++;
        return "test-token";
      },
    });
    await expect(
      client.organizations.getOrganization(
        { organizationId: ORG },
        { signal: controller.signal },
      ),
    ).rejects.toMatchObject({ code: Code.Canceled });
    expect(providerCalls).toBe(0);
    expect(server.calls).toEqual([]);
  });

  test("a token source rejection reaches a live caller before any request", async () => {
    const server = fakeServer(() => {});
    const providerError = new Error("token source failed");
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: () => Promise.reject(providerError),
    });
    const error = (await client.organizations
      .getOrganization({ organizationId: ORG })
      .catch((error: unknown) => error)) as ConnectError;
    expect(error.cause).toBeInstanceOf(TideConfigError);
    expect((error.cause as TideConfigError).cause).toBe(providerError);
    expect(server.calls).toEqual([]);
  });

  test("a token source that returns nothing fails before any request", async () => {
    const server = fakeServer(() => {});
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: async () => "",
    });
    const error = await client.organizations
      .getOrganization({ organizationId: ORG })
      .catch((e: unknown) => e);
    // Connect wraps an interceptor failure and keeps it as the cause.
    expect((error as ConnectError).cause).toBeInstanceOf(TideConfigError);
    expect(server.calls).toEqual([]);
  });

  test("a token that is not a valid header value fails without the token in the error", async () => {
    const server = fakeServer(() => {});
    for (const token of [
      "secret-access\rtoken",
      "secret-access\ntoken",
      "secret-access\0token",
      () => "secret-access\u20actoken",
    ]) {
      const client = new TideClient({
        origin: FAKE_ORIGIN,
        fetch: server.fetch,
        token,
      });
      const error = (await client.organizations
        .getOrganization({ organizationId: ORG })
        .catch((e: unknown) => e)) as ConnectError;
      expect(error).toBeInstanceOf(ConnectError);
      const cause = error.cause as Error;
      expect(cause).toBeInstanceOf(TideConfigError);
      expect(cause.cause).toBeUndefined();
      for (const text of [
        error.message,
        String(error.stack),
        cause.message,
        String(cause.stack),
      ]) {
        expect(text).not.toContain("secret-access");
      }
    }
    expect(server.calls).toEqual([]);
  });

  test("the guarded fetch refuses every other origin", async () => {
    const seen: string[] = [];
    const fetch = guardedFetch(FAKE_ORIGIN, async (input) => {
      seen.push(String(input));
      return new Response(null);
    });
    await expect(fetch("https://evil.test/x")).rejects.toThrow(TypeError);
    await expect(fetch("https://tide.test.evil.test/x")).rejects.toThrow(
      TypeError,
    );
    await fetch(`${FAKE_ORIGIN}/x`);
    expect(seen).toEqual([`${FAKE_ORIGIN}/x`]);
  });
});

describe("error mapping", () => {
  test("keeps the Connect code and message of a server error", async () => {
    const server = fakeServer((router) => {
      router.service(RunnerProfileService, {
        deleteRunnerProfile: () => {
          throw new ConnectError("profile not found", Code.NotFound);
        },
      });
      router.service(DevSessionService, {
        createDevSession: () => {
          throw new ConnectError(
            "too many dev sessions",
            Code.ResourceExhausted,
          );
        },
      });
    });
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "tide_personal_key",
    });
    const notFound = await client.runnerProfiles
      .deleteRunnerProfile({ organizationId: ORG, runnerProfileId: "x" })
      .catch((e: unknown) => e);
    expect(notFound).toBeInstanceOf(ConnectError);
    expect((notFound as ConnectError).code).toBe(Code.NotFound);
    expect((notFound as ConnectError).rawMessage).toBe("profile not found");
    const exhausted = await client.devSessions
      .createDevSession({ organizationId: ORG, runnerProfileId: "p" })
      .catch((e: unknown) => e);
    expect(connectCodeOf(exhausted)).toBe(Code.ResourceExhausted);
  });

  test("a personal API key on another service gets the server's permission_denied", async () => {
    const server = fakeServer((router) => {
      router.service(OrganizationService, {
        listOrganizations: (_req, ctx) => {
          if (
            ctx.requestHeader.get("authorization")?.startsWith("Bearer tide_")
          ) {
            throw new ConnectError(
              "personal API keys work only on DevSessionService",
              Code.PermissionDenied,
            );
          }
          return {};
        },
      });
    });
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "tide_abc",
    });
    const error = await client.organizations
      .listOrganizations({})
      .catch((e: unknown) => e);
    expect(connectCodeOf(error)).toBe(Code.PermissionDenied);
  });

  test("redacts an echoed normalized token and keeps the status", async () => {
    const server = fakeServer((router) => {
      router.service(OrganizationService, {
        getOrganization: (_req, ctx) => {
          const token = ctx.requestHeader.get("authorization")!.slice(7);
          throw new ConnectError(`bad token ${token}`, Code.Unauthenticated, {
            "x-tide-test": "rejected",
          });
        },
      });
    });
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: () => "secret-access-token\r\n",
    });
    const error = (await client.organizations
      .getOrganization({ organizationId: ORG })
      .catch((e: unknown) => e)) as ConnectError;
    expect(error).toBeInstanceOf(ConnectError);
    expect(error.code).toBe(Code.Unauthenticated);
    expect(error.metadata.get("x-tide-test")).toBe("rejected");
    expect(error.message).not.toContain("secret-access-token");
    expect(error.rawMessage).toBe("bad token [redacted]");
  });

  test.each([
    ["trailing space", "secret-access-token "],
    ["trailing LF", "secret-access-token\n"],
    ["trailing CRLF", "secret-access-token\r\n"],
    ["surrounding tabs", "\tsecret-access-token\t"],
    ["leading space", " secret-access-token"],
  ])(
    "normalizes a token with %s and redacts fetch errors",
    async (_edge, raw) => {
      for (const token of [raw, async () => raw]) {
        let auth: string | null = null;
        const client = new TideClient({
          origin: FAKE_ORIGIN,
          fetch: async (_input, init) => {
            auth = new Headers(init?.headers).get("authorization");
            throw new TypeError(`fetch failed with ${auth}`, {
              cause: new Error(`original token: ${raw}`),
            });
          },
          token,
        });
        const error = (await client.organizations
          .getOrganization({ organizationId: ORG })
          .catch((e: unknown) => e)) as ConnectError;
        expect(auth).toBe("Bearer secret-access-token");
        expect(error).toBeInstanceOf(ConnectError);
        expect(error.code).toBe(Code.Unknown);
        expect(error.rawMessage).toMatch(
          /^fetch failed with Bearer ?\[redacted\]$/,
        );
        expect(error.message).toBe(`[unknown] ${error.rawMessage}`);
        const cause = error.cause as Error;
        expect(cause).toBeInstanceOf(TypeError);
        expect(cause.message).toBe(error.rawMessage);
        expect(String(cause.stack)).not.toContain("secret-access-token");
        expect(cause.cause).toBeInstanceOf(Error);
        expect((cause.cause as Error).message).toBe(
          "original token: [redacted]",
        );
      }
    },
  );

  test("a network failure is a ConnectError that keeps the cause", async () => {
    const server = fakeServer(() => {}, { failFetch: () => true });
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    const error = await client.organizations
      .getOrganization({ organizationId: ORG })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).cause).toBeInstanceOf(TypeError);
  });

  test("connectCodeOf follows wrapped causes", () => {
    const inner = new ConnectError("x", Code.FailedPrecondition);
    expect(connectCodeOf(new TideError("outer", { cause: inner }))).toBe(
      Code.FailedPrecondition,
    );
    expect(connectCodeOf(new Error("plain"))).toBeUndefined();
  });
});
