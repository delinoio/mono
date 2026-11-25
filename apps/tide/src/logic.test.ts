// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "@rstest/core";
import {
  AgentVendor,
  GuestOs,
  Priority,
  RequestState,
  Role,
  RunnerState,
  WorkloadKind,
} from "@delino/tide-protos/v1/common_pb";
import {
  CreateRunnerProfileRequestSchema,
  UpdateRunnerProfileRequestSchema,
} from "@delino/tide-protos/v1/runner_profile_pb";
import { RequestSchema, RunnerSchema } from "@delino/tide-protos/v1/work_pb";
import { WatchQueueEventKind } from "tide-sdk";
import { nextSetupStep, SetupStep } from "./components/setup.tsx";
import { AuthMode, DEFAULT_API_ORIGIN, readConsoleConfig } from "./config.ts";
import { formatSpan, queueWaitMs } from "./format.ts";
import {
  emptyProfileForm,
  formFromProfile,
  profileErrors,
  profileFromForm,
  profileUpdateMask,
  SourceKind,
} from "./forms.ts";
import {
  parseGitHubCallback,
  stripCallbackFromAddress,
} from "./github-callback.ts";
import {
  invitationLink,
  secretFromInput,
  takeSecretFromAddress,
} from "./invitation.ts";
import {
  applyQueueEvent,
  initialQueueView,
  LiveState,
  QueueViewActionKind,
  waitingInQueueOrder,
} from "./queue.ts";
import {
  can,
  canDeleteDevSession,
  canManageMember,
  Permission,
} from "./roles.ts";
import { parseRoute, RouteId, routePath } from "./router.tsx";

describe("configuration", () => {
  describe.each([false, true])("production=%s", (production) => {
    test("normalizes a complete Logto registration and requests tide:use", () => {
      const config = readConsoleConfig(
        {
          PUBLIC_LOGTO_ENDPOINT: " https://login.example/ ",
          PUBLIC_LOGTO_APP_ID: " app ",
          PUBLIC_LOGTO_SCOPES: "read:example tide:use",
        },
        { production },
      );
      expect(config.apiOrigin).toBe(DEFAULT_API_ORIGIN);
      expect(DEFAULT_API_ORIGIN).toBe("https://tide.api.delino.io");
      expect(config.logto).toEqual({
        endpoint: "https://login.example",
        appId: "app",
        scopes: ["tide:use", "read:example"],
      });
    });

    describe.each([undefined, "", "   ", "app"])("app ID %j", (appId) => {
      test.each([
        ["not-a-url", "is not a valid URL"],
        [
          "https://login.example/path",
          "must be an origin without a path, query or fragment",
        ],
      ])("rejects Logto endpoint %j", (endpoint, error) => {
        expect(() =>
          readConsoleConfig(
            { PUBLIC_LOGTO_ENDPOINT: endpoint, PUBLIC_LOGTO_APP_ID: appId },
            { production },
          ),
        ).toThrow(`PUBLIC_LOGTO_ENDPOINT ${error}`);
      });
    });

    test.each([undefined, "", "   "])(
      "leaves sign-in unconfigured with a valid endpoint and app ID %j",
      (appId) => {
        expect(
          readConsoleConfig(
            {
              PUBLIC_LOGTO_ENDPOINT: "https://login.example",
              PUBLIC_LOGTO_APP_ID: appId,
            },
            { production },
          ).logto,
        ).toBeUndefined();
      },
    );

    test.each([undefined, "", "   "])(
      "leaves sign-in unconfigured with an app ID and endpoint %j",
      (endpoint) => {
        expect(
          readConsoleConfig(
            { PUBLIC_LOGTO_ENDPOINT: endpoint, PUBLIC_LOGTO_APP_ID: "app" },
            { production },
          ).logto,
        ).toBeUndefined();
      },
    );
  });

  const pathError = "must be an origin without a path, query or fragment";

  test.each([
    ["https://api.test\\v1\\..", pathError],
    ["https://api.test\\", pathError],
    ["https:///api.test/v1/..", pathError],
    ["https:api.test/v1/..", pathError],
    ["https:/api.test/v1/..", pathError],
    ["https:////api.test/v1/..", pathError],
    ["https://api.test/v1/..", pathError],
    ["https://api.test/v1", pathError],
    ["https://api.test?x", pathError],
    ["https://api.test?", pathError],
    ["https://api.test#x", pathError],
    ["https://api.test#", pathError],
    ["https://u:p@api.test", "must not contain credentials"],
    ["ftp://api.test", "must use http or https"],
    ["nope", "is not a valid URL"],
  ])("rejects %s for both configured origins", (value, error) => {
    expect(() =>
      readConsoleConfig({ PUBLIC_API_ORIGIN: value }, { production: false }),
    ).toThrow(`PUBLIC_API_ORIGIN ${error}`);
    expect(() =>
      readConsoleConfig(
        { PUBLIC_LOGTO_ENDPOINT: value, PUBLIC_LOGTO_APP_ID: "app" },
        { production: false },
      ),
    ).toThrow(`PUBLIC_LOGTO_ENDPOINT ${error}`);
  });

  test.each([
    ["https://api.test", "https://api.test"],
    ["https://api.test/", "https://api.test"],
    ["http://localhost:8080", "http://localhost:8080"],
    ["HTTPS://api.test", "https://api.test"],
    [" https://api.test/ ", "https://api.test"],
  ])("normalizes %j to a bare origin", (value, origin) => {
    const config = readConsoleConfig(
      {
        PUBLIC_API_ORIGIN: value,
        PUBLIC_LOGTO_ENDPOINT: value,
        PUBLIC_LOGTO_APP_ID: "app",
      },
      { production: false },
    );
    expect(config.apiOrigin).toBe(origin);
    expect(config.logto?.endpoint).toBe(origin);
  });

  test.each(["", "   ", "\t\n"])(
    "uses defaults for the blank value %j",
    (value) => {
      const config = readConsoleConfig(
        {
          PUBLIC_API_ORIGIN: value,
          PUBLIC_LOGTO_ENDPOINT: value,
          PUBLIC_LOGTO_APP_ID: "app",
        },
        { production: false },
      );
      expect(config.apiOrigin).toBe(DEFAULT_API_ORIGIN);
      expect(config.logto).toBeUndefined();
    },
  );

  test("gives the mock flag precedence over an invalid Logto endpoint", () => {
    const env = {
      PUBLIC_TIDE_MOCK_API: " true ",
      PUBLIC_LOGTO_ENDPOINT: "not-a-url",
      PUBLIC_LOGTO_APP_ID: "app",
    };
    expect(() => readConsoleConfig(env, { production: true })).toThrow(
      "PUBLIC_TIDE_MOCK_API must not be set for a production build",
    );
    expect(readConsoleConfig(env, { production: false })).toMatchObject({
      authMode: AuthMode.Mock,
      logto: undefined,
    });
  });

  test("shows sign-in as not configured without a Logto registration", () => {
    expect(readConsoleConfig({}, { production: true }).logto).toBeUndefined();
  });
});

describe("router", () => {
  test("parses every route of the contract", () => {
    const org = "o1";
    expect(parseRoute("/").id).toBe(RouteId.Organizations);
    expect(parseRoute("/invitations/accept").id).toBe(RouteId.Invitation);
    expect(parseRoute("/github/setup").id).toBe(RouteId.GitHubSetup);
    expect(parseRoute("/callback").id).toBe(RouteId.Callback);
    expect(parseRoute(`/orgs/${org}`)).toEqual({
      id: RouteId.Overview,
      organizationId: org,
    });
    for (const id of [
      RouteId.Requests,
      RouteId.Runners,
      RouteId.Profiles,
      RouteId.Integrations,
      RouteId.Members,
      RouteId.DevSessions,
      RouteId.ApiKeys,
      RouteId.Audit,
      RouteId.Settings,
    ] as const) {
      const path = routePath({ id, organizationId: org });
      expect(path).toBe(`/orgs/${org}/${id}`);
      expect(parseRoute(path)).toEqual({ id, organizationId: org });
    }
    expect(parseRoute("/orgs/o1/constructor").id).toBe(RouteId.NotFound);
    expect(parseRoute("/orgs/%/requests").id).toBe(RouteId.NotFound);
  });
});

describe("roles", () => {
  test("follows the authorization matrix", () => {
    expect(can(Role.MEMBER, Permission.ReadOrganization)).toBe(true);
    expect(can(Role.MEMBER, Permission.ManageOwnApiKeys)).toBe(true);
    expect(can(Role.MEMBER, Permission.ManageIntegrations)).toBe(false);
    expect(can(Role.MEMBER, Permission.ChangeSettings)).toBe(false);
    expect(can(Role.ADMIN, Permission.ManageRunnerProfiles)).toBe(true);
    expect(can(Role.ADMIN, Permission.DeleteOrganization)).toBe(false);
    expect(can(Role.OWNER, Permission.TransferOwnership)).toBe(true);
    expect(can(Role.OWNER, Permission.LeaveOrganization)).toBe(false);
    expect(can(undefined, Permission.ReadOrganization)).toBe(false);
  });

  test("lets the session owner and admins delete a dev session", () => {
    const session = { ownerUserId: "u1" };
    expect(canDeleteDevSession(Role.MEMBER, "u1", session)).toBe(true);
    expect(canDeleteDevSession(Role.MEMBER, "u2", session)).toBe(false);
    expect(canDeleteDevSession(Role.ADMIN, "u2", session)).toBe(true);
    expect(canDeleteDevSession(Role.OWNER, "u2", session)).toBe(true);
  });

  test("an admin cannot manage the owner or itself", () => {
    expect(
      canManageMember(Role.ADMIN, "a", { userId: "o", role: Role.OWNER }),
    ).toBe(false);
    expect(
      canManageMember(Role.ADMIN, "a", { userId: "a", role: Role.ADMIN }),
    ).toBe(false);
    expect(
      canManageMember(Role.ADMIN, "a", { userId: "m", role: Role.MEMBER }),
    ).toBe(true);
  });
});

describe("runner profile form", () => {
  test.each([
    {
      field: "vcpuCount",
      max: 1024,
      oversized: 3_000_000_000,
      requiredMessage: "Enter at least 1 vCPU.",
      rangeMessage: "Enter a whole number of vCPUs from 1 to 1024.",
    },
    {
      field: "memoryMib",
      max: 16_777_216,
      oversized: 1e19,
      requiredMessage: "Enter the memory in MiB.",
      rangeMessage: "Enter a whole number of MiB from 1 to 16777216.",
    },
    {
      field: "diskGib",
      max: 1_048_576,
      oversized: 1e19,
      requiredMessage: "Enter the disk size in GiB.",
      rangeMessage: "Enter a whole number of GiB from 1 to 1048576.",
    },
  ] as const)(
    "validates whole numbers and limits for $field",
    ({ field, max, oversized, requiredMessage, rangeMessage }) => {
      const form = { ...emptyProfileForm(), name: "linux-build" };
      for (const value of [null, 0, -1]) {
        expect(profileErrors({ ...form, [field]: value }, undefined)).toEqual({
          [field]: requiredMessage,
        });
      }
      for (const value of [1.5, max + 1, oversized]) {
        expect(profileErrors({ ...form, [field]: value }, undefined)).toEqual({
          [field]: rangeMessage,
        });
      }
      for (const value of [1, max]) {
        expect(profileErrors({ ...form, [field]: value }, undefined)).toEqual(
          {},
        );
      }
    },
  );

  test("compares the idle TTL only when both TTLs are valid", () => {
    const form = { ...emptyProfileForm(), name: "linux-build", maxTtlHours: 1 };
    expect(profileErrors({ ...form, idleTtlMinutes: 61 }, undefined)).toEqual({
      idleTtlMinutes: "The idle TTL must not be longer than the maximum TTL.",
    });
    for (const idleTtlMinutes of [59.5, 60]) {
      expect(profileErrors({ ...form, idleTtlMinutes }, undefined)).toEqual({});
    }
    for (const maxTtlHours of [null, 0, -1, 200]) {
      expect(
        profileErrors(
          { ...form, maxTtlHours, idleTtlMinutes: 1_000_000 },
          undefined,
        ),
      ).toEqual({
        maxTtlHours: expect.any(String),
      });
    }
    expect(profileErrors({ ...form, idleTtlMinutes: null }, undefined)).toEqual(
      {
        idleTtlMinutes: "Enter an idle TTL greater than zero.",
      },
    );
  });

  test.each([
    { vcpuCount: 1, memoryMib: 1, diskGib: 1 },
    { vcpuCount: 1024, memoryMib: 16_777_216, diskGib: 1_048_576 },
  ])(
    "serializes create and update at resource limits ($vcpuCount vCPUs)",
    (resources) => {
      const form = {
        ...emptyProfileForm(),
        name: "linux-build",
        idleTtlMinutes: 59.5,
        maxQueueWaitMinutes: 1.5,
        ...resources,
      };
      expect(profileErrors(form, undefined)).toEqual({});
      const profile = profileFromForm(form);
      expect(() =>
        toBinary(
          CreateRunnerProfileRequestSchema,
          create(CreateRunnerProfileRequestSchema, { profile }),
        ),
      ).not.toThrow();
      expect(() =>
        toBinary(
          UpdateRunnerProfileRequestSchema,
          create(UpdateRunnerProfileRequestSchema, {
            profile,
            updateMask: profileUpdateMask(emptyProfileForm(), form),
          }),
        ),
      ).not.toThrow();
    },
  );

  test("requires workspace_path for DEV_SESSION profiles in the guest OS syntax", () => {
    const values = {
      ...emptyProfileForm(),
      name: "win-dev",
      workloadKind: WorkloadKind.DEV_SESSION,
      guestOs: GuestOs.WINDOWS,
    };
    expect(profileErrors(values, undefined).workspacePath).toContain(
      "workspace path",
    );
    expect(
      profileErrors({ ...values, workspacePath: "/home/dev" }, undefined)
        .workspacePath,
    ).toContain("Windows");
    expect(
      profileErrors({ ...values, workspacePath: "C:\\work" }, undefined),
    ).toEqual({});
    const linux = { ...values, guestOs: GuestOs.LINUX, workspacePath: "work" };
    expect(profileErrors(linux, undefined).workspacePath).toBeDefined();
  });

  test("does not send workspace_path for other kinds", () => {
    const p = profileFromForm({
      ...emptyProfileForm(),
      name: "linux-build",
      workspacePath: "/ignored",
    });
    expect(p.workspacePath).toBe("");
    expect(p.source).toEqual({ case: "baseImage", value: GuestOs.LINUX });
  });

  test("checks the name, the agent integration and the OpenAI queue wait", () => {
    const base = { ...emptyProfileForm(), name: "Bad Name" };
    expect(profileErrors(base, undefined).name).toBeDefined();
    const agent = {
      ...emptyProfileForm(),
      name: "codex",
      workloadKind: WorkloadKind.AGENT_SESSION,
    };
    expect(profileErrors(agent, undefined).agentIntegrationId).toBeDefined();
    const codex = {
      ...agent,
      agentIntegrationId: "i1",
      maxQueueWaitMinutes: 10,
    };
    expect(
      profileErrors(codex, AgentVendor.OPENAI_CODEX).maxQueueWaitMinutes,
    ).toContain("5 minutes");
    expect(
      profileErrors(
        { ...codex, maxQueueWaitMinutes: 5 },
        AgentVendor.OPENAI_CODEX,
      ),
    ).toEqual({});
    expect(
      profileErrors(
        { ...codex, maxQueueWaitMinutes: 5, guestOs: GuestOs.WINDOWS },
        AgentVendor.CLAUDE_CODE,
      ).guestOs,
    ).toBeDefined();
  });

  test("masks only the changed mutable fields", () => {
    const before = formFromProfile(
      profileFromForm({ ...emptyProfileForm(), name: "linux-build" }),
    );
    expect(profileUpdateMask(before, before).paths).toEqual([]);
    const after = {
      ...before,
      warmCount: 2,
      priority: Priority.LOW,
      sourceKind: SourceKind.Template,
      templateId: "t1",
    };
    expect(profileUpdateMask(before, after).paths).toEqual([
      "source",
      "priority",
      "warm_count",
    ]);
  });
});

describe("queue view", () => {
  const request = (
    id: string,
    state: RequestState,
    priority = Priority.NORMAL,
    queuedMs = 0,
  ) =>
    create(RequestSchema, {
      requestId: id,
      state,
      priority,
      queuedAt: timestampFromMs(queuedMs),
    });

  test("builds the snapshot and then applies changes", () => {
    let view = initialQueueView();
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Request,
      request: request("a", RequestState.QUEUED),
    });
    expect(view.live).toBe(LiveState.Connecting);
    expect(view.requests.size).toBe(0);
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.SnapshotComplete,
    });
    expect(view.live).toBe(LiveState.Live);
    expect([...view.requests.keys()]).toEqual(["a"]);
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Request,
      request: request("a", RequestState.CANCELED),
    });
    expect(view.requests.size).toBe(0);
  });

  test("keeps the loading state when the stream fails before the first snapshot", () => {
    const view = initialQueueView();
    expect(
      applyQueueEvent(view, { kind: QueueViewActionKind.StreamFailed }),
    ).toBe(view);
  });

  test("keeps the last state across a gap and replaces it with the new snapshot", () => {
    let view = initialQueueView();
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Runner,
      runner: create(RunnerSchema, { runnerId: "w1", state: RunnerState.WARM }),
    });
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.SnapshotComplete,
    });
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Gap,
      failures: 0,
      delayMs: 1000,
      cause: undefined,
    });
    expect(view.live).toBe(LiveState.Reconnecting);
    expect([...view.runners.keys()]).toEqual(["w1"]);
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Runner,
      runner: create(RunnerSchema, { runnerId: "w2", state: RunnerState.BUSY }),
    });
    expect([...view.runners.keys()]).toEqual(["w1"]);
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.SnapshotComplete,
    });
    expect(view.live).toBe(LiveState.Live);
    expect([...view.runners.keys()]).toEqual(["w2"]);
    expect(view.reconnects).toBe(1);
  });

  test("a restart replaces the last state with the snapshot of the new stream", () => {
    let view = initialQueueView();
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Runner,
      runner: create(RunnerSchema, { runnerId: "w1", state: RunnerState.WARM }),
    });
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.SnapshotComplete,
    });
    // A manual retry opens a new iterator, which sends no gap.
    view = applyQueueEvent(view, { kind: QueueViewActionKind.Restart });
    expect(view.live).toBe(LiveState.Reconnecting);
    expect([...view.runners.keys()]).toEqual(["w1"]);
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.Runner,
      runner: create(RunnerSchema, { runnerId: "w2", state: RunnerState.BUSY }),
    });
    view = applyQueueEvent(view, {
      kind: WatchQueueEventKind.SnapshotComplete,
    });
    expect(view.live).toBe(LiveState.Live);
    expect([...view.runners.keys()]).toEqual(["w2"]);
    expect(view.reconnects).toBe(0);
  });

  test("orders waiting requests by priority, queue time and ID", () => {
    const order = waitingInQueueOrder([
      request("n-old", RequestState.QUEUED, Priority.NORMAL, 1),
      request("h-new", RequestState.QUEUED, Priority.HIGH, 50),
      request("running", RequestState.RUNNING, Priority.HIGH, 0),
      request("n-b", RequestState.QUEUED, Priority.NORMAL, 9),
      request("n-a", RequestState.QUEUED, Priority.NORMAL, 9),
      request("low", RequestState.QUEUED, Priority.LOW, 0),
    ]).map((r) => r.requestId);
    expect(order).toEqual(["h-new", "n-old", "n-a", "n-b", "low"]);
  });

  test("orders waiting requests within the same millisecond by queue time", () => {
    const later = create(RequestSchema, {
      requestId: "n-a",
      state: RequestState.QUEUED,
      priority: Priority.NORMAL,
      queuedAt: { seconds: 1_700_000_000n, nanos: 123_900_000 },
    });
    const earlier = create(RequestSchema, {
      requestId: "n-b",
      state: RequestState.QUEUED,
      priority: Priority.NORMAL,
      queuedAt: { seconds: 1_700_000_000n, nanos: 123_100_000 },
    });

    expect(
      waitingInQueueOrder([later, earlier]).map((r) => r.requestId),
    ).toEqual(["n-b", "n-a"]);
  });

  test("treats a missing queue timestamp as the Unix epoch", () => {
    const missing = create(RequestSchema, {
      requestId: "a-missing",
      state: RequestState.QUEUED,
      priority: Priority.NORMAL,
    });
    const order = waitingInQueueOrder([
      request("b-epoch", RequestState.QUEUED),
      missing,
      request("c-before-epoch", RequestState.QUEUED, Priority.NORMAL, -1),
    ]).map((r) => r.requestId);
    expect(order).toEqual(["c-before-epoch", "a-missing", "b-epoch"]);
  });

  test("measures the queue wait until start, end or now", () => {
    expect(queueWaitMs({ queuedAt: timestampFromMs(1000) }, 61_000)).toBe(
      60_000,
    );
    expect(
      queueWaitMs(
        { queuedAt: timestampFromMs(0), startedAt: timestampFromMs(5000) },
        99_999,
      ),
    ).toBe(5000);
    expect(formatSpan(3_900_000)).toBe("1 h 5 min");
  });
});

describe("setup guide", () => {
  test("names the steps in the contract order", () => {
    const none = {
      linked: false,
      hasGitHubInstallation: false,
      hasAgentIntegration: false,
      hasRunnerProfile: false,
    };
    expect(nextSetupStep(none)).toBe(SetupStep.LinkOwnSand);
    expect(nextSetupStep({ ...none, linked: true })).toBe(
      SetupStep.ConnectWorkSource,
    );
    expect(
      nextSetupStep({ ...none, linked: true, hasAgentIntegration: true }),
    ).toBe(SetupStep.CreateRunnerProfile);
    expect(
      nextSetupStep({
        linked: true,
        hasGitHubInstallation: true,
        hasAgentIntegration: false,
        hasRunnerProfile: true,
      }),
    ).toBe(SetupStep.Done);
  });
});

describe("GitHub callback", () => {
  test("parses installation_id, code and state", () => {
    expect(
      parseGitHubCallback(
        "?installation_id=42&code=c1&state=s1&setup_action=install",
      ),
    ).toEqual({ githubInstallationId: 42n, code: "c1", state: "s1" });
    expect(parseGitHubCallback("?installation_id=42&state=s1")).toBeUndefined();
    expect(
      parseGitHubCallback("?installation_id=x&code=c&state=s"),
    ).toBeUndefined();
  });

  test("removes the query from the address bar", () => {
    const calls: string[] = [];
    stripCallbackFromAddress(
      { pathname: "/github/setup", search: "?code=c&state=s", hash: "" },
      { replaceState: (_d, _u, url) => calls.push(String(url)) },
    );
    expect(calls).toEqual(["/github/setup"]);
  });
});

describe("invitation links", () => {
  test("round-trips the secret through the fragment and strips it", () => {
    const link = invitationLink("https://tide.delino.io", "tdi_abc");
    expect(link).toBe(
      "https://tide.delino.io/invitations/accept#secret=tdi_abc",
    );
    expect(secretFromInput(link)).toBe("tdi_abc");
    expect(secretFromInput("tdi_abc")).toBe("tdi_abc");
    expect(secretFromInput("two words")).toBeUndefined();
    const calls: string[] = [];
    expect(
      takeSecretFromAddress(
        {
          pathname: "/invitations/accept",
          search: "",
          hash: "#secret=tdi_abc",
        },
        { replaceState: (_d, _u, url) => calls.push(String(url)) },
      ),
    ).toBe("tdi_abc");
    expect(calls).toEqual(["/invitations/accept"]);
  });
});
