// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  AgentActivity,
  BridgeErrorKind,
  PermissionOptionKind,
  PermissionOutcome,
  StopReason,
  ToolCallStatus,
  ToolKind,
} from "@delino/sandcrew-protos/v1/bridge_pb";
import { PermissionMode } from "@delino/sandcrew-protos/v1/common_pb";
import { OrganizationSettingsSchema } from "@delino/sandcrew-protos/v1/organization_pb";
import {
  type ListSessionsResponse,
  ListSessionsResponseSchema,
  SessionSchema,
  type TranscriptEvent,
  TranscriptEventSchema,
} from "@delino/sandcrew-protos/v1/session_pb";
import type { InfiniteData } from "@tanstack/react-query";
import { beforeEach, describe, expect, test } from "@rstest/core";
import { uuidV7 } from "./api.ts";
import {
  authJsonError,
  MAX_AUTH_JSON_BYTES,
} from "./components/credentials.tsx";
import { canManageOrganization, visibleSections } from "./components/shell.tsx";
import { AuthMode, readConsoleConfig, REQUIRED_API_SCOPE } from "./config.ts";
import { diffLines, DiffLineKind, MAX_DIFF_CELLS } from "./diff.ts";
import { safeHref } from "./markdown.tsx";
import { updateSessionPages } from "./pages/session-list.tsx";
import {
  draftFromSettings,
  settingsUpdate,
  validateDraft,
} from "./pages/settings-organization.tsx";
import { secretNameError } from "./pages/settings-user.tsx";
import { parseRoute, RouteId, routePath } from "./router.tsx";
import {
  readSelectedOrganization,
  readSessionFilter,
  SessionFilter,
  writeSelectedOrganization,
  writeSessionFilter,
} from "./storage.ts";
import { isTerminalStreamError, reconnectDelayMs } from "./streams.ts";
import {
  buildTranscript,
  ChatItemKind,
  isTurnRunning,
  mergeEvents,
} from "./transcript.ts";
import {
  base64UrlToBytes,
  browserLabel,
  bytesToBase64Url,
  type PushEnvironment,
  PushOptInError,
  PushOptInFailure,
  PushSupport,
  pushSupport,
  subscribeToPush,
} from "./web-push.ts";
import { Role } from "@delino/sandcrew-protos/v1/common_pb";

type EventInit = MessageInitShape<typeof TranscriptEventSchema>["event"];

function events(...cases: EventInit[]): TranscriptEvent[] {
  return cases.map((event, index) =>
    create(TranscriptEventSchema, { sequence: BigInt(index + 1), event }),
  );
}

const text = (value: string) => ({
  content: { block: { case: "text" as const, value } },
});

describe("configuration", () => {
  test("requires the sandcrew:use scope and the SandCrew API origin", () => {
    const config = readConsoleConfig(
      {
        PUBLIC_LOGTO_ENDPOINT: "https://auth.example.com",
        PUBLIC_LOGTO_APP_ID: "app",
        PUBLIC_LOGTO_SCOPES: "openid sandcrew:use",
      },
      { production: true },
    );
    expect(config.apiOrigin).toBe("https://sandcrew.api.delino.io");
    expect(config.logto?.scopes).toEqual([REQUIRED_API_SCOPE, "openid"]);
    expect(REQUIRED_API_SCOPE).toBe("sandcrew:use");
  });

  test("rejects the mock API in a production build", () => {
    expect(() =>
      readConsoleConfig(
        { PUBLIC_SANDCREW_MOCK_API: "true" },
        { production: true },
      ),
    ).toThrow("PUBLIC_SANDCREW_MOCK_API must not be set");
    expect(
      readConsoleConfig(
        { PUBLIC_SANDCREW_MOCK_API: " true " },
        { production: false },
      ).authMode,
    ).toBe(AuthMode.Mock);
  });

  test("rejects an origin with a path", () => {
    expect(() =>
      readConsoleConfig(
        { PUBLIC_API_ORIGIN: "https://api.example.com/v1" },
        { production: false },
      ),
    ).toThrow("PUBLIC_API_ORIGIN must be an origin");
  });
});

describe("router", () => {
  const org = "0194c000-0000-7000-8000-000000000001";
  test.each([
    ["/", { id: RouteId.OrganizationPick }],
    ["/callback", { id: RouteId.SignInCallback }],
    [`/orgs/${org}/sessions`, { id: RouteId.SessionList, organizationId: org }],
    [
      `/orgs/${org}/sessions/new`,
      { id: RouteId.SessionNew, organizationId: org },
    ],
    [
      `/orgs/${org}/sessions/abc`,
      { id: RouteId.SessionChat, organizationId: org, sessionId: "abc" },
    ],
    [
      `/orgs/${org}/settings`,
      { id: RouteId.SettingsUser, organizationId: org },
    ],
    [
      `/orgs/${org}/settings/organization`,
      { id: RouteId.SettingsOrg, organizationId: org },
    ],
    [`/orgs/${org}`, { id: RouteId.NotFound }],
    [`/orgs/${org}/settings/other`, { id: RouteId.NotFound }],
    [`/orgs/${org}/sessions/a/b`, { id: RouteId.NotFound }],
    ["/orgs/%E0%A4%A", { id: RouteId.NotFound }],
  ])("parses %s", (path, route) => {
    expect(parseRoute(path)).toEqual(route);
  });

  test("builds every organization route back to its path", () => {
    for (const path of [
      `/orgs/${org}/sessions`,
      `/orgs/${org}/sessions/new`,
      `/orgs/${org}/sessions/s1`,
      `/orgs/${org}/settings`,
      `/orgs/${org}/settings/organization`,
    ]) {
      expect(routePath(parseRoute(path))).toBe(path);
    }
  });

  test("shows the organization settings only to owners and admins", () => {
    expect(canManageOrganization(Role.OWNER)).toBe(true);
    expect(canManageOrganization(Role.ADMIN)).toBe(true);
    expect(canManageOrganization(Role.MEMBER)).toBe(false);
    expect(visibleSections(Role.MEMBER).map((s) => s.id)).not.toContain(
      RouteId.SettingsOrg,
    );
    expect(visibleSections(Role.ADMIN).map((s) => s.id)).toContain(
      RouteId.SettingsOrg,
    );
  });
});

describe("transcript", () => {
  test("joins consecutive chunks of the same kind into one item", () => {
    const t = buildTranscript(
      events(
        { case: "userMessageChunk", value: text("Hello ") },
        { case: "userMessageChunk", value: text("agent") },
        { case: "agentThoughtChunk", value: text("thinking") },
        { case: "agentMessageChunk", value: text("Hi") },
        { case: "agentMessageChunk", value: text("!") },
      ),
    );
    expect(t.items.map((i) => i.kind)).toEqual([
      ChatItemKind.UserMessage,
      ChatItemKind.AgentThought,
      ChatItemKind.AgentMessage,
    ]);
    const first = t.items[0]!;
    expect(first.kind === ChatItemKind.UserMessage && first.blocks).toEqual([
      expect.objectContaining({
        block: { case: "text", value: "Hello agent" },
      }),
    ]);
  });

  test("applies tool call updates in place", () => {
    const t = buildTranscript(
      events(
        {
          case: "toolCall",
          value: {
            toolCallId: "t1",
            title: "Run tests",
            kind: ToolKind.EXECUTE,
            status: ToolCallStatus.PENDING,
            rawInput: "{}",
          },
        },
        { case: "agentMessageChunk", value: text("Running") },
        {
          case: "toolCallUpdate",
          value: {
            toolCallId: "t1",
            status: ToolCallStatus.COMPLETED,
            rawOutput: '{"ok":true}',
            content: {
              items: [
                {
                  content: { case: "terminal", value: { terminalId: "term" } },
                },
              ],
            },
          },
        },
      ),
    );
    expect(t.items).toHaveLength(2);
    expect(t.items[0]).toMatchObject({
      kind: ChatItemKind.ToolCall,
      title: "Run tests",
      status: ToolCallStatus.COMPLETED,
      rawInput: "{}",
      rawOutput: '{"ok":true}',
    });
  });

  test("keeps only the latest plan", () => {
    const t = buildTranscript(
      events(
        { case: "plan", value: { entries: [{ content: "one" }] } },
        { case: "agentMessageChunk", value: text("x") },
        { case: "plan", value: { entries: [{ content: "two" }] } },
      ),
    );
    expect(t.items.filter((i) => i.kind === ChatItemKind.Plan)).toEqual([
      expect.objectContaining({
        entries: [expect.objectContaining({ content: "two" })],
      }),
    ]);
    expect(t.items.at(-1)?.kind).toBe(ChatItemKind.Plan);
  });

  test("tracks pending permission requests and auto-approved answers", () => {
    const t = buildTranscript(
      events(
        {
          case: "toolCall",
          value: { toolCallId: "t1", title: "rm -rf build" },
        },
        {
          case: "permissionRequest",
          value: {
            permissionRequestId: "p1",
            toolCallId: "t1",
            options: [
              {
                optionId: "a",
                name: "Allow",
                kind: PermissionOptionKind.ALLOW_ONCE,
              },
            ],
          },
        },
        {
          case: "permissionResponse",
          value: {
            permissionRequestId: "p1",
            outcome: PermissionOutcome.SELECTED,
            optionId: "a",
            autoApproved: true,
          },
        },
        {
          case: "permissionRequest",
          value: { permissionRequestId: "p2", toolCallId: "t1" },
        },
        {
          case: "sessionState",
          value: { state: AgentActivity.WAITING_FOR_PERMISSION },
        },
      ),
    );
    expect(t.pendingPermissionIds).toEqual(["p2"]);
    expect(t.items[1]).toMatchObject({
      kind: ChatItemKind.Permission,
      toolTitle: "rm -rf build",
      response: expect.objectContaining({ autoApproved: true }),
    });
    expect(isTurnRunning(t)).toBe(true);
  });

  test("shows the tool call details that come only with a permission request", () => {
    const t = buildTranscript(
      events({
        case: "permissionRequest",
        value: {
          permissionRequestId: "p1",
          toolCallId: "t1",
          options: [
            {
              optionId: "a",
              name: "Allow",
              kind: PermissionOptionKind.ALLOW_ONCE,
            },
          ],
          toolCall: {
            toolCallId: "t1",
            title: "rm -rf build",
            kind: ToolKind.EXECUTE,
            status: ToolCallStatus.PENDING,
            rawInput: '{"command":"rm -rf build"}',
          },
        },
      }),
    );
    expect(t.items).toHaveLength(2);
    expect(t.items[0]).toMatchObject({
      kind: ChatItemKind.ToolCall,
      toolCallId: "t1",
      title: "rm -rf build",
      toolKind: ToolKind.EXECUTE,
      rawInput: '{"command":"rm -rf build"}',
    });
    expect(t.items[1]).toMatchObject({
      kind: ChatItemKind.Permission,
      toolTitle: "rm -rf build",
    });
    expect(new Set(t.items.map((i) => i.key)).size).toBe(2);
    expect(t.pendingPermissionIds).toEqual(["p1"]);
  });

  test("merges permission request details into an earlier tool call", () => {
    const t = buildTranscript(
      events(
        {
          case: "toolCall",
          value: { toolCallId: "t1", title: "Run", rawInput: "{}" },
        },
        {
          case: "permissionRequest",
          value: {
            permissionRequestId: "p1",
            toolCallId: "t1",
            toolCall: { toolCallId: "t1", title: "rm -rf build" },
          },
        },
      ),
    );
    expect(t.items).toHaveLength(2);
    expect(t.items[0]).toMatchObject({
      kind: ChatItemKind.ToolCall,
      title: "rm -rf build",
      rawInput: "{}",
    });
    expect(t.items[1]).toMatchObject({ toolTitle: "rm -rf build" });
  });

  test("records modes, options, errors, exits and unknown updates", () => {
    const t = buildTranscript(
      events(
        {
          case: "modeChanged",
          value: {
            currentModeId: "default",
            availableModes: [
              { modeId: "default", name: "Default" },
              { modeId: "plan", name: "Plan" },
            ],
            permissionMode: PermissionMode.AUTO_APPROVE,
          },
        },
        {
          case: "configOptions",
          value: { options: [{ configOptionId: "model", name: "Model" }] },
        },
        {
          case: "modeChanged",
          value: {
            currentModeId: "plan",
            availableModes: [
              { modeId: "default", name: "Default" },
              { modeId: "plan", name: "Plan" },
            ],
            permissionMode: PermissionMode.ASK,
          },
        },
        {
          case: "bridgeError",
          value: {
            kind: BridgeErrorKind.AGENT_INSTALL_FAILED,
            message: "download",
          },
        },
        { case: "inputRejected", value: { inputId: "i" } },
        { case: "turnEnded", value: { stopReason: StopReason.CANCELLED } },
        {
          case: "agentExited",
          value: { result: { case: "signal", value: "SIGKILL" } },
        },
        {
          case: "unknownUpdate",
          value: { sessionUpdate: "usage_update", rawJson: "{}" },
        },
        { case: "hello", value: {} },
        { case: "agentReady", value: {} },
      ),
    );
    expect(t.currentModeId).toBe("plan");
    expect(t.permissionMode).toBe(PermissionMode.ASK);
    expect(t.configOptions.map((o) => o.configOptionId)).toEqual(["model"]);
    expect(t.agentInstallFailed).toBe(true);
    expect(t.items.map((i) => i.kind)).toEqual([
      ChatItemKind.ModeChanged,
      ChatItemKind.BridgeError,
      ChatItemKind.InputRejected,
      ChatItemKind.TurnEnded,
      ChatItemKind.AgentExited,
      ChatItemKind.Unknown,
    ]);
    expect(t.items[0]).toMatchObject({ modeName: "Plan" });
    expect(t.lastSequence).toBe(10n);
  });

  test("merges events once, in sequence order", () => {
    const [a, b, c] = events(
      { case: "agentMessageChunk", value: text("a") },
      { case: "agentMessageChunk", value: text("b") },
      { case: "agentMessageChunk", value: text("c") },
    );
    const first = mergeEvents([], [c!, a!]);
    expect(first.map((e) => e.sequence)).toEqual([1n, 3n]);
    const second = mergeEvents(first, [b!, c!]);
    expect(second.map((e) => e.sequence)).toEqual([1n, 2n, 3n]);
    expect(mergeEvents(second, [a!])).toBe(second);
  });
});

describe("markdown", () => {
  test("allows only http, https and mailto links", () => {
    expect(safeHref("https://example.com/x")).toBe("https://example.com/x");
    expect(safeHref("mailto:a@example.com")).toBe("mailto:a@example.com");
    expect(safeHref("javascript:alert(1)")).toBeUndefined();
    expect(safeHref("data:text/html,x")).toBeUndefined();
    expect(safeHref("/relative")).toBeUndefined();
  });
});

describe("diff", () => {
  test("marks removed and added lines around common lines", () => {
    expect(diffLines("a\nb\nc\n", "a\nx\nc\n")).toEqual([
      { kind: DiffLineKind.Same, text: "a" },
      { kind: DiffLineKind.Removed, text: "b" },
      { kind: DiffLineKind.Added, text: "x" },
      { kind: DiffLineKind.Same, text: "c" },
    ]);
  });

  test("shows every line of a new file as added", () => {
    expect(diffLines(undefined, "a\nb")).toEqual([
      { kind: DiffLineKind.Added, text: "a" },
      { kind: DiffLineKind.Added, text: "b" },
    ]);
  });

  test("does not search a very large diff", () => {
    const big = Array.from(
      { length: Math.sqrt(MAX_DIFF_CELLS) + 1 },
      (_, i) => `l${i}`,
    ).join("\n");
    const lines = diffLines(big, big);
    expect(lines.every((l) => l.kind !== DiffLineKind.Same)).toBe(true);
  });
});

describe("session list cache", () => {
  const session = (id: string, label: string) =>
    create(SessionSchema, { sessionId: id, label });
  const pages = (): InfiniteData<ListSessionsResponse> => ({
    pageParams: [""],
    pages: [
      create(ListSessionsResponseSchema, {
        sessions: [session("a", "A"), session("b", "B")],
      }),
    ],
  });

  test("replaces a changed session", () => {
    const result = updateSessionPages(pages(), "b", session("b", "B2"));
    expect(result.found).toBe(true);
    expect(result.data?.pages[0]?.sessions.map((s) => s.label)).toEqual([
      "A",
      "B2",
    ]);
  });

  test("removes a deleted session", () => {
    const result = updateSessionPages(pages(), "a", undefined);
    expect(result.data?.pages[0]?.sessions.map((s) => s.sessionId)).toEqual([
      "b",
    ]);
  });

  test("reports an unknown session", () => {
    const data = pages();
    const result = updateSessionPages(data, "c", session("c", "C"));
    expect(result).toEqual({ data, found: false });
  });
});

describe("organization settings", () => {
  const saved = draftFromSettings(
    create(OrganizationSettingsSchema, {
      allowedRunnerProfileIds: ["p1"],
      pauseDelay: durationFromMs(10 * 60_000),
      pausedSessionRetention: {
        retention: { case: "duration", value: durationFromMs(30 * 86_400_000) },
      },
      updateTime: timestampFromMs(0),
    }),
  );

  test("reads the settings into a draft", () => {
    expect(saved).toEqual({
      allowedRunnerProfileIds: ["p1"],
      pauseDelayMinutes: 10,
      retention: "days",
      retentionDays: 30,
    });
  });

  test("sends only the changed paths", () => {
    expect(settingsUpdate(saved, saved)).toBeUndefined();
    const update = settingsUpdate(saved, { ...saved, pauseDelayMinutes: 15 });
    expect(update?.paths).toEqual(["pause_delay"]);
    const unlimited = settingsUpdate(saved, {
      ...saved,
      allowedRunnerProfileIds: ["p1", "p2"],
      retention: "unlimited" as typeof saved.retention,
    });
    expect(unlimited?.paths).toEqual([
      "allowed_runner_profile_ids",
      "paused_session_retention",
    ]);
    expect(unlimited?.settings.pausedSessionRetention).toEqual({
      retention: { case: "unlimited", value: true },
    });
  });

  test("validates the ranges of the control plane", () => {
    expect(validateDraft(saved, saved)).toEqual({});
    expect(
      validateDraft({ ...saved, pauseDelayMinutes: 0 }, saved).pauseDelay,
    ).toBeDefined();
    expect(
      validateDraft({ ...saved, pauseDelayMinutes: 1441 }, saved).pauseDelay,
    ).toBeDefined();
    expect(
      validateDraft({ ...saved, pauseDelayMinutes: 1.5 }, saved).pauseDelay,
    ).toBeDefined();
    expect(
      validateDraft({ ...saved, retentionDays: 366 }, saved).retentionDays,
    ).toBeDefined();
    expect(
      validateDraft(
        {
          ...saved,
          retention: "unlimited" as typeof saved.retention,
          retentionDays: null,
        },
        saved,
      ),
    ).toEqual({});
  });

  test("accepts an unchanged whole-second pause delay of the server", () => {
    // 90 seconds is valid for the control plane but not a whole minute.
    const server = { ...saved, pauseDelayMinutes: 1.5 };
    expect(validateDraft({ ...server, retentionDays: 60 }, server)).toEqual({});
  });
});

describe("credentials and secrets", () => {
  test("accepts only a JSON object of at most 64 KiB as auth.json", () => {
    const encode = (s: string) => new TextEncoder().encode(s);
    expect(authJsonError(encode('{"tokens":{}}'))).toBeUndefined();
    expect(authJsonError(encode("[]"))).toBe("credentials.error.notObject");
    expect(authJsonError(encode("{"))).toBe("credentials.error.notJson");
    expect(authJsonError(new Uint8Array(MAX_AUTH_JSON_BYTES + 1))).toBe(
      "credentials.error.fileTooLarge",
    );
  });

  test("checks session secret names", () => {
    expect(secretNameError("GITHUB_TOKEN")).toBeUndefined();
    expect(secretNameError("_x1")).toBeUndefined();
    expect(secretNameError("1X")).toBeDefined();
    expect(secretNameError("A-B")).toBeDefined();
    expect(secretNameError("SANDCREW_TOKEN")).toBe(
      "secrets.error.nameReserved",
    );
    expect(secretNameError("A".repeat(129))).toBeDefined();
  });
});

describe("storage", () => {
  beforeEach(() => window.localStorage.clear());

  test("remembers the organization and the filter", () => {
    expect(readSessionFilter()).toBe(SessionFilter.Mine);
    writeSessionFilter(SessionFilter.All);
    expect(readSessionFilter()).toBe(SessionFilter.All);
    writeSelectedOrganization("org");
    expect(readSelectedOrganization()).toBe("org");
    writeSelectedOrganization(undefined);
    expect(readSelectedOrganization()).toBeUndefined();
  });
});

describe("streams", () => {
  test("stops only after terminal errors", () => {
    expect(isTerminalStreamError(new ConnectError("x", Code.NotFound))).toBe(
      true,
    );
    expect(
      isTerminalStreamError(new ConnectError("x", Code.Unauthenticated)),
    ).toBe(true);
    expect(isTerminalStreamError(new ConnectError("x", Code.Unavailable))).toBe(
      false,
    );
    expect(isTerminalStreamError(new Error("network"))).toBe(false);
  });

  test("backs off up to 30 seconds", () => {
    expect([0, 1, 2, 5, 10].map(reconnectDelayMs)).toEqual([
      1_000, 2_000, 4_000, 30_000, 30_000,
    ]);
  });
});

describe("ids", () => {
  test("creates UUID v7 values", () => {
    const id = uuidV7(Date.UTC(2026, 0, 1));
    expect(id).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
    expect(parseInt(id.slice(0, 8) + id.slice(9, 13), 16)).toBe(
      Date.UTC(2026, 0, 1),
    );
  });
});

describe("web push", () => {
  function fakeEnvironment(
    permission: NotificationPermission,
    answer: NotificationPermission,
  ) {
    const calls: string[] = [];
    const env: PushEnvironment = {
      hasPushManager: true,
      notification: {
        permission,
        requestPermission: async () => {
          calls.push("requestPermission");
          return answer;
        },
      },
      serviceWorker: {
        register: async (url: string | URL) => {
          calls.push(`register ${String(url)}`);
          return {
            pushManager: {
              subscribe: async (options: PushSubscriptionOptionsInit) => {
                calls.push(
                  `subscribe ${(options.applicationServerKey as Uint8Array).byteLength}`,
                );
                return {
                  endpoint: "https://push.example.com/abc",
                  getKey: (name: string) =>
                    new Uint8Array(name === "auth" ? [1, 2, 3] : [4, 5, 6])
                      .buffer,
                };
              },
            },
          } as unknown as ServiceWorkerRegistration;
        },
      },
    };
    return { env, calls };
  }

  test("reports unsupported and blocked browsers", () => {
    expect(
      pushSupport({
        hasPushManager: false,
        notification: undefined,
        serviceWorker: undefined,
      }),
    ).toBe(PushSupport.Unsupported);
    expect(pushSupport(fakeEnvironment("denied", "denied").env)).toBe(
      PushSupport.Denied,
    );
    expect(pushSupport(fakeEnvironment("default", "granted").env)).toBe(
      PushSupport.Available,
    );
  });

  test("asks for permission, registers the service worker and subscribes", async () => {
    const { env, calls } = fakeEnvironment("default", "granted");
    const subscribing = subscribeToPush(async () => {
      calls.push("load key");
      return bytesToBase64Url(new Uint8Array(65).buffer);
    }, env);
    // The permission request starts in the click's task, before the key
    // loads: Safari shows the prompt only during transient activation.
    expect(calls).toEqual(["requestPermission"]);
    const keys = await subscribing;
    expect(calls).toEqual([
      "requestPermission",
      "load key",
      "register /sw.js",
      "subscribe 65",
    ]);
    expect(keys).toEqual({
      endpoint: "https://push.example.com/abc",
      p256dhKey: "BAUG",
      authKey: "AQID",
    });
  });

  test("subscribes only after a new service worker is active", async () => {
    const calls: string[] = [];
    const worker = new EventTarget() as EventTarget & {
      state: ServiceWorkerState;
    };
    worker.state = "installing";
    const env: PushEnvironment = {
      hasPushManager: true,
      notification: {
        permission: "granted",
        requestPermission: async () => "granted",
      },
      serviceWorker: {
        register: async () =>
          ({
            active: null,
            installing: worker,
            waiting: null,
            pushManager: {
              subscribe: async () => {
                calls.push(`subscribe while ${worker.state}`);
                return {
                  endpoint: "https://push.example.com/abc",
                  getKey: () => new Uint8Array([1]).buffer,
                };
              },
            },
          }) as unknown as ServiceWorkerRegistration,
      },
    };
    const subscribing = subscribeToPush(async () => "AAAA", env);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(calls).toEqual([]);
    worker.state = "installed";
    worker.dispatchEvent(new Event("statechange"));
    worker.state = "activated";
    worker.dispatchEvent(new Event("statechange"));
    await subscribing;
    expect(calls).toEqual(["subscribe while activated"]);
  });

  test("stops when the user does not grant permission", async () => {
    const { env, calls } = fakeEnvironment("default", "denied");
    const error = await subscribeToPush(async () => "AAAA", env).catch(
      (e: unknown) => e,
    );
    expect(error).toBeInstanceOf(PushOptInError);
    expect((error as PushOptInError).failure).toBe(
      PushOptInFailure.PermissionNotGranted,
    );
    expect(calls).toEqual(["requestPermission"]);
  });

  test("converts base64url keys", () => {
    expect([...base64UrlToBytes("AQID")]).toEqual([1, 2, 3]);
    expect([...base64UrlToBytes("-_8")]).toEqual([251, 255]);
    expect(bytesToBase64Url(new Uint8Array([251, 255]).buffer)).toBe("-_8");
  });

  test("names the browser", () => {
    expect(
      browserLabel(
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
      ),
    ).toBe("Safari on macOS");
    expect(
      browserLabel(
        "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0",
      ),
    ).toBe("Firefox on Linux");
  });
});
