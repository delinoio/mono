// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import {
  durationFromMs,
  DurationSchema,
  timestampFromMs,
  timestampMs,
} from "@bufbuild/protobuf/wkt";
import { Code, createClient } from "@connectrpc/connect";
import {
  AgentCredential,
  AgentIntegrationService,
} from "@delino/sandrun-protos/v1/agent_integration_pb";
import { ApiKeyService } from "@delino/sandrun-protos/v1/api_key_pb";
import {
  AuditAction,
  AuditService,
  AuditTargetKind,
} from "@delino/sandrun-protos/v1/audit_pb";
import {
  AgentVendor,
  ApiKeyState,
  RequestEndReason,
  RequestState,
  ResourcesSchema,
  Role,
  RunnerEndReason,
  RunnerState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { MemberService } from "@delino/sandrun-protos/v1/member_pb";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/sandrun-protos/v1/runner_profile_pb";
import {
  type Request,
  type WatchQueueResponse,
  WorkService,
} from "@delino/sandrun-protos/v1/work_pb";
import { expect, rs, test } from "@rstest/core";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
  MOCK_USER,
} from "./mock-api.ts";

test("ends the departing member's queued dev session and publishes one update", async () => {
  const transport = createMockTransport();
  const organizationId = MOCK_IDS.acme;
  const members = createClient(MemberService, transport);
  const organizations = createClient(OrganizationService, transport);
  const sessions = createClient(DevSessionService, transport);
  const work = createClient(WorkService, transport);
  const getSession = (devSessionId: string) =>
    sessions.getDevSession({ organizationId, devSessionId });
  const getRequest = (requestId: string) =>
    work.getRequest({ organizationId, requestId });

  const { invitationSecret } = await members.createInvitation({
    organizationId,
    role: Role.ADMIN,
  });
  await organizations.transferOwnership({
    organizationId,
    userId: "mock-user-ada",
  });
  const queuedJob = await getRequest(MOCK_IDS.queuedJob);
  expect(queuedJob.request?.state).toBe(RequestState.QUEUED);
  const endedSession = await getSession(MOCK_IDS.devSessionOther);
  expect(endedSession.devSession?.state).toBe(RequestState.ENDED);

  const watch = new AbortController();
  const stream = work
    .watchQueue({ organizationId }, { signal: watch.signal })
    [Symbol.asyncIterator]();
  try {
    const initialRequestIds: string[] = [];
    for (;;) {
      const { value, done } = await stream.next();
      if (done) throw new Error("Expected a complete snapshot");
      if (value.event.case === "snapshotComplete") break;
      if (value.event.case === "request")
        initialRequestIds.push(value.event.value.requestId);
    }
    expect(initialRequestIds).toContain(MOCK_IDS.queuedDev);

    await members.removeMember({ organizationId, userId: MOCK_USER });
    await expect(getSession(MOCK_IDS.queuedDev)).rejects.toMatchObject({
      code: Code.NotFound,
    });
    await expect(getRequest(MOCK_IDS.queuedDev)).rejects.toMatchObject({
      code: Code.NotFound,
    });
    const { member } = await members.acceptInvitation({ invitationSecret });
    expect(member).toMatchObject({ userId: MOCK_USER, role: Role.ADMIN });

    const terminal = {
      state: RequestState.CANCELED,
      endReason: RequestEndReason.MEMBER_REMOVED,
    };
    const { devSession } = await getSession(MOCK_IDS.queuedDev);
    const { request } = await getRequest(MOCK_IDS.queuedDev);
    expect(devSession).toMatchObject(terminal);
    expect(request).toMatchObject(terminal);
    expect(request?.endedAt).toBeDefined();
    expect(devSession?.endedAt).toEqual(request?.endedAt);
    expect(await getRequest(MOCK_IDS.queuedJob)).toEqual(queuedJob);
    expect(await getSession(MOCK_IDS.devSessionOther)).toEqual(endedSession);

    // Publish another update to check that removal emitted exactly one.
    const { request: canceledAgent } = await work.cancelRequest({
      organizationId,
      requestId: MOCK_IDS.queuedAgent,
    });
    for (const update of [request, canceledAgent]) {
      await expect(stream.next()).resolves.toMatchObject({
        done: false,
        value: { event: { case: "request", value: update } },
      });
    }
  } finally {
    watch.abort();
    await stream.return?.();
  }

  const rejoinedRequestIds: string[] = [];
  for await (const { event } of work.watchQueue({ organizationId })) {
    if (event.case === "snapshotComplete") break;
    if (event.case === "request")
      rejoinedRequestIds.push(event.value.requestId);
  }
  expect(rejoinedRequestIds).toContain(MOCK_IDS.queuedJob);
  expect(rejoinedRequestIds).not.toContain(MOCK_IDS.queuedDev);
});

const invalidInstallationState = {
  code: Code.FailedPrecondition,
  rawMessage: "the installation state is not valid or has expired",
};

test("rejects undefined kind filters on runner profiles and requests", async () => {
  const transport = createMockTransport();
  const profiles = createClient(RunnerProfileService, transport);
  const work = createClient(WorkService, transport);
  const organizationId = MOCK_IDS.acme;
  const unknownKind = 99 as WorkloadKind;

  await expect(
    profiles.listRunnerProfiles({ organizationId, workloadKind: unknownKind }),
  ).rejects.toMatchObject({
    code: Code.InvalidArgument,
    rawMessage: "invalid workload_kind: must be a defined WorkloadKind value",
  });
  await expect(
    work.listRequests({ organizationId, kind: unknownKind }),
  ).rejects.toMatchObject({
    code: Code.InvalidArgument,
    rawMessage: "invalid kind: must be a defined WorkloadKind value",
  });
});

test.each([
  { includeEnded: false, mixed: false },
  { includeEnded: false, mixed: true },
  { includeEnded: true, mixed: false },
  { includeEnded: true, mixed: true },
])(
  "rejects undefined state filters (includeEnded=$includeEnded, mixed=$mixed)",
  async ({ includeEnded, mixed }) => {
    const transport = createMockTransport();
    const work = createClient(WorkService, transport);
    const sessions = createClient(DevSessionService, transport);
    const request = { organizationId: MOCK_IDS.acme, includeEnded };
    const requestStates = mixed ? [RequestState.QUEUED] : [];
    requestStates.push(99 as RequestState);
    const runnerStates = mixed ? [RunnerState.BUSY] : [];
    runnerStates.push(99 as RunnerState);
    const invalidRequestState = {
      code: Code.InvalidArgument,
      rawMessage: "invalid states: must be a defined RequestState value",
    };

    await expect(
      work.listRequests({ ...request, states: requestStates }),
    ).rejects.toMatchObject(invalidRequestState);
    await expect(
      work.listRunners({ ...request, states: runnerStates }),
    ).rejects.toMatchObject({
      code: Code.InvalidArgument,
      rawMessage: "invalid states: must be a defined RunnerState value",
    });
    await expect(
      sessions.listDevSessions({ ...request, states: requestStates }),
    ).rejects.toMatchObject(invalidRequestState);
  },
);

test("keeps defined filters, zero values and omitted filters working", async () => {
  const transport = createMockTransport();
  const profiles = createClient(RunnerProfileService, transport);
  const work = createClient(WorkService, transport);
  const sessions = createClient(DevSessionService, transport);
  const request = { organizationId: MOCK_IDS.acme };

  const allProfiles = await profiles.listRunnerProfiles(request);
  expect(allProfiles.profiles.map((profile) => profile.workloadKind)).toEqual([
    WorkloadKind.GITHUB_JOB,
    WorkloadKind.AGENT_SESSION,
    WorkloadKind.DEV_SESSION,
  ]);
  expect(
    await profiles.listRunnerProfiles({
      ...request,
      workloadKind: WorkloadKind.UNSPECIFIED,
    }),
  ).toEqual(allProfiles);
  const githubProfiles = await profiles.listRunnerProfiles({
    ...request,
    workloadKind: WorkloadKind.GITHUB_JOB,
  });
  expect(
    githubProfiles.profiles.map((profile) => profile.runnerProfileId),
  ).toEqual([MOCK_IDS.linuxProfile]);

  const githubRequests = await work.listRequests({
    ...request,
    kind: WorkloadKind.GITHUB_JOB,
    states: [RequestState.QUEUED],
  });
  expect(githubRequests.requests.map((row) => row.requestId)).toEqual([
    MOCK_IDS.queuedJob,
  ]);
  expect(
    await work.listRequests({ ...request, kind: WorkloadKind.UNSPECIFIED }),
  ).toEqual(await work.listRequests(request));
  const busyRunners = await work.listRunners({
    ...request,
    states: [RunnerState.BUSY],
  });
  expect(busyRunners.runners.map((runner) => runner.runnerId)).toEqual([
    MOCK_IDS.busyRunner,
  ]);
  const queuedSessions = await sessions.listDevSessions({
    ...request,
    states: [RequestState.QUEUED],
  });
  expect(
    queuedSessions.devSessions.map((session) => session.devSessionId),
  ).toEqual([MOCK_IDS.queuedDev]);
  expect(
    await work.listRequests({ ...request, states: [RequestState.UNSPECIFIED] }),
  ).toMatchObject({ requests: [] });
  expect(
    await work.listRunners({ ...request, states: [RunnerState.UNSPECIFIED] }),
  ).toMatchObject({ runners: [] });
  expect(
    await sessions.listDevSessions({
      ...request,
      states: [RequestState.UNSPECIFIED],
    }),
  ).toMatchObject({ devSessions: [] });
});

test("paginates filtered requests in queue order before ended requests", async () => {
  const work = createClient(WorkService, createMockTransport());
  const request = {
    organizationId: MOCK_IDS.acme,
    kind: WorkloadKind.GITHUB_JOB,
    includeEnded: true,
    pageSize: 2,
  };
  const first = await work.listRequests(request);
  expect(first.requests.map((row) => row.requestId)).toEqual([
    MOCK_IDS.runningJob,
    MOCK_IDS.queuedJob,
  ]);
  expect(first.nextPageToken).not.toBe("");

  const last = await work.listRequests({
    ...request,
    pageToken: first.nextPageToken,
  });
  expect(last.requests.map((row) => row.requestId)).toEqual([
    MOCK_IDS.endedJob,
  ]);
  expect(last.nextPageToken).toBe("");
});

async function runnerProfileFixture() {
  const transport = createMockTransport();
  const profiles = createClient(RunnerProfileService, transport);
  const audit = createClient(AuditService, transport);
  const organizationId = MOCK_IDS.acme;
  const seededRequest = {
    organizationId,
    runnerProfileId: MOCK_IDS.linuxProfile,
  };
  const { profile: seeded } = await profiles.getRunnerProfile(seededRequest);
  if (!seeded?.resources)
    throw new Error("Expected a seeded profile with resources");
  const state = async () => ({
    profiles: await profiles.listRunnerProfiles({
      organizationId,
      pageSize: 200,
    }),
    audit: await audit.listAuditEvents({ organizationId, pageSize: 200 }),
  });
  return {
    profiles,
    organizationId,
    seededRequest,
    seeded,
    resources: seeded.resources,
    state,
  };
}

test.each([
  [{ vcpuCount: 1025 }, "vcpu_count", 1024],
  [{ memoryMib: 16_777_217n }, "memory_mib", 16_777_216],
  [{ diskGib: 1_048_577n }, "disk_gib", 1_048_576],
] as const)(
  "rejects over-limit resources %s on runner profile create and update",
  async (overLimit, field, limit) => {
    const {
      profiles,
      organizationId,
      seededRequest,
      seeded,
      resources,
      state,
    } = await runnerProfileFixture();
    const invalidResources = { ...resources, ...overLimit };
    const error = {
      code: Code.InvalidArgument,
      rawMessage: `invalid resources.${field}: must be at most ${limit}`,
    };
    const before = await state();

    await expect(
      profiles.createRunnerProfile({
        organizationId,
        profile: {
          ...seeded,
          name: "over-resource-limit",
          resources: invalidResources,
        },
      }),
    ).rejects.toMatchObject(error);
    expect(await state()).toEqual(before);

    await expect(
      profiles.updateRunnerProfile({
        ...seededRequest,
        profile: { resources: invalidResources },
        updateMask: { paths: ["resources"] },
      }),
    ).rejects.toMatchObject(error);
    expect(await state()).toEqual(before);
  },
);

const invalidIdleTtl = {
  code: Code.InvalidArgument,
  rawMessage: "invalid ttl: invalid idle_ttl: must not be greater than max_ttl",
};

test("rejects a runner profile with idle TTL one millisecond above maximum TTL", async () => {
  const { profiles, organizationId, seeded, state } =
    await runnerProfileFixture();
  const before = await state();

  await expect(
    profiles.createRunnerProfile({
      organizationId,
      profile: {
        ...seeded,
        name: "over-idle-ttl-limit",
        idleTtl: durationFromMs(60_001),
        maxTtl: durationFromMs(60_000),
      },
    }),
  ).rejects.toMatchObject(invalidIdleTtl);
  expect(await state()).toEqual(before);
});

test.each([
  ["idle_ttl", { idleTtl: durationFromMs(60_001) }],
  ["max_ttl", { maxTtl: durationFromMs(59_999) }],
] as const)(
  "validates a runner profile's resulting TTLs when updating only %s",
  async (path, profile) => {
    const { profiles, seededRequest, state } = await runnerProfileFixture();
    const ttl = durationFromMs(60_000);
    await profiles.updateRunnerProfile({
      ...seededRequest,
      profile: { idleTtl: ttl, maxTtl: ttl },
      updateMask: { paths: ["idle_ttl", "max_ttl"] },
    });
    const before = await state();

    await expect(
      profiles.updateRunnerProfile({
        ...seededRequest,
        profile,
        updateMask: { paths: [path] },
      }),
    ).rejects.toMatchObject(invalidIdleTtl);
    expect(await state()).toEqual(before);
  },
);

test("creates and updates runner profiles at the resource limits with equal positive TTLs", async () => {
  const { profiles, organizationId, seededRequest, seeded } =
    await runnerProfileFixture();
  const ttl = durationFromMs(1);
  const limits = {
    resources: create(ResourcesSchema, {
      vcpuCount: 1024,
      memoryMib: 16_777_216n,
      diskGib: 1_048_576n,
    }),
    idleTtl: ttl,
    maxTtl: ttl,
  };
  const { profile: created } = await profiles.createRunnerProfile({
    organizationId,
    profile: { ...seeded, ...limits, name: "resource-limits" },
  });
  if (!created) throw new Error("Expected the created profile");
  await profiles.updateRunnerProfile({
    ...seededRequest,
    profile: limits,
    updateMask: { paths: ["resources", "idle_ttl", "max_ttl"] },
  });

  for (const runnerProfileId of [
    created.runnerProfileId,
    seededRequest.runnerProfileId,
  ]) {
    const { profile } = await profiles.getRunnerProfile({
      organizationId,
      runnerProfileId,
    });
    expect(profile).toMatchObject(limits);
  }
});

test.each([
  {
    name: "a 500 ms idle TTL",
    values: { idleTtl: create(DurationSchema, { nanos: 500_000_000 }) },
    paths: ["idle_ttl"],
  },
  {
    name: "subsecond idle and maximum TTLs",
    values: {
      idleTtl: create(DurationSchema, { nanos: 1_000_000 }),
      maxTtl: create(DurationSchema, { nanos: 500_000_000 }),
    },
    paths: ["idle_ttl", "max_ttl"],
  },
  {
    name: "a 1 ms queue wait",
    values: { maxQueueWait: create(DurationSchema, { nanos: 1_000_000 }) },
    paths: ["max_queue_wait"],
  },
  {
    name: "the exact maximum TTL and queue wait caps",
    values: {
      maxTtl: create(DurationSchema, { seconds: 604_800n }),
      maxQueueWait: create(DurationSchema, { seconds: 86_400n }),
    },
    paths: ["max_ttl", "max_queue_wait"],
  },
])(
  "preserves $name on runner profile create and update",
  async ({ values, paths }) => {
    const { profiles, organizationId, seededRequest, seeded } =
      await runnerProfileFixture();
    const { profile: created } = await profiles.createRunnerProfile({
      organizationId,
      profile: { ...seeded, ...values, name: "precise-durations" },
    });
    if (!created) throw new Error("Expected the created profile");
    expect(created).toMatchObject(values);
    expect(
      (
        await profiles.getRunnerProfile({
          organizationId,
          runnerProfileId: created.runnerProfileId,
        })
      ).profile,
    ).toMatchObject(values);

    const { profile: updated } = await profiles.updateRunnerProfile({
      ...seededRequest,
      profile: values,
      updateMask: { paths },
    });
    expect(updated).toMatchObject(values);
    expect(
      (await profiles.getRunnerProfile(seededRequest)).profile,
    ).toMatchObject(values);
  },
);

const durationFields = [
  ["idleTtl", "idle_ttl"],
  ["maxTtl", "max_ttl"],
  ["maxQueueWait", "max_queue_wait"],
] as const;

test.each(
  durationFields
    .flatMap(([field, path]) => [
      { field, path, name: "missing", components: undefined },
      { field, path, name: "zero", components: { seconds: 0n } },
      {
        field,
        path,
        name: "negative",
        components: { seconds: -1n },
      },
      {
        field,
        path,
        name: "above the protobuf seconds bound",
        components: { seconds: 315_576_000_001n },
      },
      ...[1, -1, -1_000_000, 1_000_000_000].map((nanos) => ({
        field,
        path,
        name: `${nanos} invalid nanos`,
        components: { nanos },
      })),
    ])
    .concat([
      {
        field: "maxTtl",
        path: "max_ttl",
        name: "1 ms above 7 days",
        components: { seconds: 604_800n, nanos: 1_000_000 },
      },
      {
        field: "maxQueueWait",
        path: "max_queue_wait",
        name: "1 ms above 24 hours",
        components: { seconds: 86_400n, nanos: 1_000_000 },
      },
    ]),
)(
  "rejects $path with $name without changing profiles or audits",
  async ({ field, path, components }) => {
    const { profiles, organizationId, seededRequest, seeded, state } =
      await runnerProfileFixture();
    const duration =
      components === undefined
        ? undefined
        : create(DurationSchema, {
            seconds: seeded[field]?.seconds ?? 0n,
            ...components,
          });
    const before = await state();
    const error = { code: Code.InvalidArgument };
    await expect(
      profiles.createRunnerProfile({
        organizationId,
        profile: { ...seeded, name: "invalid-duration", [field]: duration },
      }),
    ).rejects.toMatchObject(error);
    expect(await state()).toEqual(before);

    await expect(
      profiles.updateRunnerProfile({
        ...seededRequest,
        profile: { [field]: duration },
        updateMask: { paths: [path] },
      }),
    ).rejects.toMatchObject(error);
    expect(await state()).toEqual(before);
  },
);

test("request states override includeEnded before pagination", async () => {
  const work = createClient(WorkService, createMockTransport());
  const organizationId = MOCK_IDS.acme;
  const terminalStates = [RequestState.ENDED];
  const mixedStates = [RequestState.RUNNING, RequestState.ENDED];
  const activeIds = [
    MOCK_IDS.runningJob,
    MOCK_IDS.queuedJob,
    MOCK_IDS.queuedAgent,
    MOCK_IDS.queuedDev,
  ];

  for (const includeEnded of [undefined, false, true]) {
    const terminal = await work.listRequests({
      organizationId,
      includeEnded,
      states: terminalStates,
    });
    expect(terminal.requests.map((request) => request.requestId)).toEqual([
      MOCK_IDS.endedJob,
    ]);
    const mixed = await work.listRequests({
      organizationId,
      includeEnded,
      states: mixedStates,
    });
    expect(mixed.requests.map((request) => request.requestId)).toEqual([
      MOCK_IDS.runningJob,
      MOCK_IDS.endedJob,
    ]);
    const defaults = await work.listRequests({ organizationId, includeEnded });
    expect(defaults.requests.map((request) => request.requestId)).toEqual(
      includeEnded ? [...activeIds, MOCK_IDS.endedJob] : activeIds,
    );
  }

  for (const filter of [
    { runnerProfileId: MOCK_IDS.devProfile },
    { kind: WorkloadKind.DEV_SESSION },
  ]) {
    const result = await work.listRequests({
      organizationId,
      states: terminalStates,
      ...filter,
    });
    expect(result.requests).toEqual([]);
  }

  const page = { organizationId, states: mixedStates, pageSize: 1 };
  const first = await work.listRequests(page);
  expect(first.requests.map((request) => request.requestId)).toEqual([
    MOCK_IDS.runningJob,
  ]);
  expect(first.nextPageToken).not.toBe("");
  const last = await work.listRequests({
    ...page,
    pageToken: first.nextPageToken,
  });
  expect(last.requests.map((request) => request.requestId)).toEqual([
    MOCK_IDS.endedJob,
  ]);
  expect(last.nextPageToken).toBe("");
});

test("runner states override includeEnded and respect the profile filter", async () => {
  const work = createClient(WorkService, createMockTransport());
  const organizationId = MOCK_IDS.acme;
  const terminalStates = [RunnerState.DELETED];
  const activeIds = [MOCK_IDS.busyRunner, MOCK_IDS.warmRunner];

  for (const includeEnded of [undefined, false, true]) {
    const terminal = await work.listRunners({
      organizationId,
      includeEnded,
      states: terminalStates,
    });
    expect(terminal.runners.map((runner) => runner.runnerId)).toEqual([
      MOCK_IDS.deletedRunner,
    ]);
    const mixed = await work.listRunners({
      organizationId,
      includeEnded,
      states: [RunnerState.BUSY, RunnerState.DELETED],
    });
    expect(mixed.runners.map((runner) => runner.runnerId)).toEqual([
      MOCK_IDS.busyRunner,
      MOCK_IDS.deletedRunner,
    ]);
    const defaults = await work.listRunners({ organizationId, includeEnded });
    expect(defaults.runners.map((runner) => runner.runnerId)).toEqual(
      includeEnded ? [...activeIds, MOCK_IDS.deletedRunner] : activeIds,
    );
  }

  const otherProfile = await work.listRunners({
    organizationId,
    states: terminalStates,
    runnerProfileId: MOCK_IDS.devProfile,
  });
  expect(otherProfile.runners).toEqual([]);
});

test("dev session states override includeEnded and respect owner and profile filters", async () => {
  const sessions = createClient(DevSessionService, createMockTransport());
  const organizationId = MOCK_IDS.acme;
  const terminalStates = [RequestState.ENDED];
  const mixedIds = [MOCK_IDS.queuedDev, MOCK_IDS.devSessionOther];

  for (const includeEnded of [undefined, false, true]) {
    const terminal = await sessions.listDevSessions({
      organizationId,
      includeEnded,
      states: terminalStates,
    });
    expect(terminal.devSessions.map((session) => session.devSessionId)).toEqual(
      [MOCK_IDS.devSessionOther],
    );
    const mixed = await sessions.listDevSessions({
      organizationId,
      includeEnded,
      states: [RequestState.QUEUED, RequestState.ENDED],
    });
    expect(mixed.devSessions.map((session) => session.devSessionId)).toEqual(
      mixedIds,
    );
    const defaults = await sessions.listDevSessions({
      organizationId,
      includeEnded,
    });
    expect(defaults.devSessions.map((session) => session.devSessionId)).toEqual(
      includeEnded ? mixedIds : [MOCK_IDS.queuedDev],
    );
  }

  const filters = {
    organizationId,
    states: terminalStates,
    includeEnded: false,
  };
  const { devSession } = await sessions.getDevSession({
    organizationId,
    devSessionId: MOCK_IDS.devSessionOther,
  });
  if (!devSession) throw new Error("Expected the seeded ended dev session");
  const matchingOwner = await sessions.listDevSessions({
    ...filters,
    owner: { case: "ownerUserId", value: devSession.ownerUserId },
  });
  expect(
    matchingOwner.devSessions.map((session) => session.devSessionId),
  ).toEqual([MOCK_IDS.devSessionOther]);
  const caller = await sessions.listDevSessions({
    ...filters,
    owner: { case: "ownedByCaller", value: true },
  });
  expect(caller.devSessions).toEqual([]);
  const otherProfile = await sessions.listDevSessions({
    ...filters,
    runnerProfileId: MOCK_IDS.linuxProfile,
  });
  expect(otherProfile.devSessions).toEqual([]);
});

test("listing personal API keys reports expiry at the current time", async () => {
  const issuedAt = Date.UTC(2026, 0, 1);
  const expiresAt = timestampFromMs(issuedAt + 2_250);
  const clock = rs.spyOn(Date, "now").mockReturnValue(issuedAt);
  try {
    const apiKeys = createClient(ApiKeyService, createMockTransport());
    const organizationId = MOCK_IDS.acme;
    const { apiKey: expiring } = await apiKeys.createApiKey({
      organizationId,
      name: "expiring",
      expiresAt,
    });
    const { apiKey: permanent } = await apiKeys.createApiKey({
      organizationId,
      name: "permanent",
    });
    const { apiKey: revocable } = await apiKeys.createApiKey({
      organizationId,
      name: "revocable",
      expiresAt,
    });
    if (!revocable) throw new Error("Expected a key to revoke");
    const { apiKey: revoked } = await apiKeys.revokeApiKey({
      organizationId,
      apiKeyId: revocable.apiKeyId,
    });

    const firstPage = await apiKeys.listApiKeys({
      organizationId,
      pageSize: 1,
    });
    expect(firstPage.apiKeys.map((key) => key.apiKeyId)).toEqual([
      MOCK_IDS.apiKey,
    ]);
    expect(firstPage.nextPageToken).toBe("1");

    for (const [offsetMs, state] of [
      [-1, ApiKeyState.ACTIVE],
      [0, ApiKeyState.EXPIRED],
      [1, ApiKeyState.EXPIRED],
    ] as const) {
      clock.mockReturnValue(timestampMs(expiresAt) + offsetMs);
      const page = await apiKeys.listApiKeys({
        organizationId,
        pageToken: firstPage.nextPageToken,
        pageSize: 1,
      });
      expect(page.apiKeys).toEqual([{ ...expiring, expiresAt, state }]);
      expect(page.nextPageToken).toBe("2");
    }

    const lastPage = await apiKeys.listApiKeys({
      organizationId,
      pageToken: "2",
      pageSize: 2,
    });
    expect(lastPage.apiKeys).toEqual([
      { ...permanent, state: ApiKeyState.ACTIVE },
      { ...revoked, expiresAt, state: ApiKeyState.REVOKED },
    ]);
    expect(lastPage.nextPageToken).toBe("");
  } finally {
    clock.mockRestore();
  }
});

test("repeating an API key revocation preserves its timestamp and audit event", async () => {
  const revokedAt = Date.UTC(2026, 0, 1);
  const clock = rs.spyOn(Date, "now").mockReturnValue(revokedAt);
  try {
    const transport = createMockTransport();
    const apiKeys = createClient(ApiKeyService, transport);
    const audit = createClient(AuditService, transport);
    const organizationId = MOCK_IDS.acme;
    const { apiKey } = await apiKeys.createApiKey({
      organizationId,
      name: "repeat-revoke",
    });
    if (!apiKey) throw new Error("Expected a key to revoke");
    const { apiKeyId } = apiKey;
    const input = { organizationId, apiKeyId };

    const first = await apiKeys.revokeApiKey(input);
    expect(first.apiKey).toEqual({
      ...apiKey,
      state: ApiKeyState.REVOKED,
      revokedAt: timestampFromMs(revokedAt),
    });
    const afterFirst = await audit.listAuditEvents({ organizationId });
    const revocations = afterFirst.events.filter(
      (event) =>
        event.action === AuditAction.API_KEY_REVOKED &&
        event.targetId === apiKeyId,
    );
    expect(revocations).toHaveLength(1);
    expect(revocations[0]).toMatchObject({
      targetKind: AuditTargetKind.API_KEY,
      subjectUserId: MOCK_USER,
      createTime: timestampFromMs(revokedAt),
    });

    clock.mockReturnValue(revokedAt + 1000);
    expect(await apiKeys.revokeApiKey(input)).toEqual(first);
    const { apiKeys: listed } = await apiKeys.listApiKeys({ organizationId });
    expect(listed.find((key) => key.apiKeyId === apiKeyId)).toEqual(
      first.apiKey,
    );
    expect(await audit.listAuditEvents({ organizationId })).toEqual(afterFirst);

    await expect(
      apiKeys.revokeApiKey({ ...input, apiKeyId: "unknown-key" }),
    ).rejects.toMatchObject({ code: Code.NotFound });
    await expect(
      apiKeys.revokeApiKey({ ...input, organizationId: MOCK_IDS.contoso }),
    ).rejects.toMatchObject({
      code: Code.NotFound,
      rawMessage: "organization not found",
    });
    expect(await audit.listAuditEvents({ organizationId })).toEqual(afterFirst);
  } finally {
    clock.mockRestore();
  }
});

const apiKeyClock = Date.UTC(2026, 0, 1) + 250;
const apiKeyClockTimestamp = timestampFromMs(apiKeyClock);

const linuxProfile = {
  organizationId: MOCK_IDS.acme,
  runnerProfileId: MOCK_IDS.linuxProfile,
};
test.each([
  { label: "an omitted update mask", mask: {} },
  {
    label: "an update mask with zero paths",
    mask: { updateMask: { paths: [] } },
  },
])(
  "rejects $label without changing the profile or its audit history",
  async ({ mask }) => {
    const transport = createMockTransport();
    const profiles = createClient(RunnerProfileService, transport);
    const audit = createClient(AuditService, transport);
    const auditRequest = { organizationId: linuxProfile.organizationId };
    const before = structuredClone(
      await profiles.getRunnerProfile(linuxProfile),
    );
    const auditsBefore = structuredClone(
      await audit.listAuditEvents(auditRequest),
    );

    await expect(
      profiles.updateRunnerProfile({
        ...linuxProfile,
        profile: { name: "linux-build-new" },
        ...mask,
      }),
    ).rejects.toMatchObject({
      code: Code.InvalidArgument,
      rawMessage: "invalid update_mask: must name at least one path",
    });

    expect(await profiles.getRunnerProfile(linuxProfile)).toEqual(before);
    expect(await audit.listAuditEvents(auditRequest)).toEqual(auditsBefore);
  },
);

test("updates the runner profile name and label and adds one audit event", async () => {
  const transport = createMockTransport();
  const profiles = createClient(RunnerProfileService, transport);
  const audit = createClient(AuditService, transport);
  const auditRequest = { organizationId: linuxProfile.organizationId };
  const auditsBefore = structuredClone(
    await audit.listAuditEvents(auditRequest),
  );

  const updated = await profiles.updateRunnerProfile({
    ...linuxProfile,
    profile: { name: "linux-build-new" },
    updateMask: { paths: ["name"] },
  });

  expect(updated.profile).toMatchObject({
    name: "linux-build-new",
    githubLabel: "sandrun-linux-build-new",
  });
  expect((await profiles.getRunnerProfile(linuxProfile)).profile).toEqual(
    updated.profile,
  );
  const auditAfter = await audit.listAuditEvents(auditRequest);
  const [event, ...events] = auditAfter.events;
  expect(event).toMatchObject({
    action: AuditAction.RUNNER_PROFILE_UPDATED,
    targetKind: AuditTargetKind.RUNNER_PROFILE,
    targetId: linuxProfile.runnerProfileId,
  });
  expect({ ...auditAfter, events }).toEqual(auditsBefore);
});

const seededResources = { vcpuCount: 4, memoryMib: 8192n, diskGib: 64n };

test.each([
  {
    paths: ["resources.vcpu_count"],
    resources: { vcpuCount: 8 },
  },
  {
    paths: ["resources.memory_mib"],
    resources: { memoryMib: 12288n },
  },
  {
    paths: ["resources.disk_gib"],
    resources: { diskGib: 96n },
  },
  {
    paths: ["resources.memory_mib", "resources.disk_gib"],
    resources: { memoryMib: 12288n, diskGib: 96n },
  },
  {
    paths: ["resources"],
    resources: { vcpuCount: 8, memoryMib: 12288n, diskGib: 96n },
  },
])(
  "updates the runner profile with mask $paths",
  async ({ paths, resources }) => {
    const profiles = createClient(RunnerProfileService, createMockTransport());

    const { profile } = await profiles.updateRunnerProfile({
      ...linuxProfile,
      profile: { resources },
      updateMask: { paths },
    });

    const expected = { ...seededResources, ...resources };
    expect(profile?.resources).toMatchObject(expected);
    expect((await profiles.getRunnerProfile(linuxProfile)).profile).toEqual(
      profile,
    );
  },
);

test.each([
  {
    label: "a zero masked value",
    paths: ["resources.memory_mib"],
    resources: { memoryMib: 0n },
    rawMessage: "resources must be greater than zero",
  },
  {
    label: "a missing resources message",
    paths: ["resources.memory_mib"],
    resources: undefined,
    rawMessage: "resources must be greater than zero",
  },
  {
    label: "an omitted value after a valid masked value",
    paths: ["resources.memory_mib", "resources.disk_gib"],
    resources: { memoryMib: 12288n },
    rawMessage: "resources must be greater than zero",
  },
  {
    label: "an unknown path after a valid masked value",
    paths: ["resources.memory_mib", "resources.unsupported"],
    resources: { memoryMib: 12288n },
    rawMessage: "unknown mask path resources.unsupported",
  },
])(
  "rejects a resource mask with $label without changing the profile or its audit history",
  async ({ paths, resources, rawMessage }) => {
    const transport = createMockTransport();
    const profiles = createClient(RunnerProfileService, transport);
    const audit = createClient(AuditService, transport);
    const auditRequest = { organizationId: linuxProfile.organizationId };
    const before = structuredClone(
      await profiles.getRunnerProfile(linuxProfile),
    );
    const auditsBefore = structuredClone(
      await audit.listAuditEvents(auditRequest),
    );

    await expect(
      profiles.updateRunnerProfile({
        ...linuxProfile,
        profile: { resources },
        updateMask: { paths },
      }),
    ).rejects.toMatchObject({ code: Code.InvalidArgument, rawMessage });

    expect(await profiles.getRunnerProfile(linuxProfile)).toEqual(before);
    expect(await audit.listAuditEvents(auditRequest)).toEqual(auditsBefore);
  },
);

test.each([
  {
    label: "one nanosecond in the future",
    expiresAt: { ...apiKeyClockTimestamp, nanos: 250_000_001 },
  },
  {
    label: "later in the same second",
    expiresAt: { ...apiKeyClockTimestamp, nanos: 750_000_000 },
  },
  {
    label: "in the next second",
    expiresAt: timestampFromMs(Date.UTC(2026, 0, 1) + 1000),
  },
  { label: "omitted", expiresAt: undefined },
])("creates an API key with expiry $label", async ({ expiresAt }) => {
  const clock = rs.spyOn(Date, "now").mockReturnValue(apiKeyClock);
  try {
    const apiKeys = createClient(ApiKeyService, createMockTransport());
    const organizationId = MOCK_IDS.acme;
    const name = "new fixture key";
    const { apiKey } = await apiKeys.createApiKey({
      organizationId,
      name,
      expiresAt,
    });
    expect(apiKey?.name).toBe(name);
    expect(apiKey?.expiresAt).toEqual(expiresAt);

    const listed = await apiKeys.listApiKeys({ organizationId });
    expect(listed.apiKeys.find((key) => key.name === name)).toEqual(apiKey);
  } finally {
    clock.mockRestore();
  }
});

test.each([
  { label: "at the current time", nanos: 250_000_000 },
  { label: "one nanosecond in the past", nanos: 249_999_999 },
])(
  "rejects an API key expiry $label without creating it",
  async ({ nanos }) => {
    const clock = rs.spyOn(Date, "now").mockReturnValue(apiKeyClock);
    try {
      const apiKeys = createClient(ApiKeyService, createMockTransport());
      const organizationId = MOCK_IDS.acme;
      const before = await apiKeys.listApiKeys({ organizationId });

      await expect(
        apiKeys.createApiKey({
          organizationId,
          name: "rejected fixture key",
          expiresAt: { ...apiKeyClockTimestamp, nanos },
        }),
      ).rejects.toMatchObject({
        code: Code.InvalidArgument,
        rawMessage: "expires_at must be in the future",
      });
      expect(await apiKeys.listApiKeys({ organizationId })).toEqual(before);
    } finally {
      clock.mockRestore();
    }
  },
);

test("capacity refreshes device counts after a runner is deleted", async () => {
  const organizationId = MOCK_IDS.acme;
  const work = createClient(WorkService, createMockTransport());
  const { devices: before } = await work.getCapacity({ organizationId });
  expect(
    before.map(({ ownsandDeviceId, runners }) => [
      ownsandDeviceId,
      Object.fromEntries(runners.map(({ state, count }) => [state, count])),
    ]),
  ).toEqual([
    [MOCK_IDS.linuxDevice, { [RunnerState.WARM]: 1, [RunnerState.BUSY]: 1 }],
    [MOCK_IDS.macDevice, {}],
    [MOCK_IDS.windowsDevice, {}],
  ]);

  const { runner } = await work.deleteRunner({
    organizationId,
    runnerId: MOCK_IDS.warmRunner,
  });
  expect(runner?.state).toBe(RunnerState.DELETED);

  const { devices: after } = await work.getCapacity({ organizationId });
  expect(after).toEqual(
    before.map((device) =>
      device.ownsandDeviceId === MOCK_IDS.linuxDevice
        ? {
            ...device,
            runners: device.runners.filter(
              (count) => count.state === RunnerState.BUSY,
            ),
          }
        : device,
    ),
  );
});

test("capacity groups only open runners assigned to devices in the organization", async () => {
  const organizationId = MOCK_IDS.acme;
  const ownsandDeviceId = MOCK_IDS.macDevice;
  const work = createClient(
    WorkService,
    createMockTransport({
      workFixtures: [
        {
          organizationId,
          runners: [
            ...[
              RunnerState.DELETING,
              RunnerState.BUSY,
              RunnerState.WAITING_FOR_WORK,
              RunnerState.WARM,
              RunnerState.WARM,
              RunnerState.PREPARING,
              RunnerState.CREATING,
              RunnerState.FAILED,
              RunnerState.UNSPECIFIED,
            ].map((state, index) => ({
              runnerId: `mac-runner-${index}`,
              ownsandDeviceId,
              state,
            })),
            { runnerId: "unassigned", state: RunnerState.WARM },
          ],
        },
        {
          organizationId: MOCK_IDS.globex,
          runners: [
            { runnerId: "other-org", ownsandDeviceId, state: RunnerState.WARM },
          ],
        },
      ],
    }),
  );
  const { devices } = await work.getCapacity({ organizationId });
  expect(
    devices.find((device) => device.ownsandDeviceId === ownsandDeviceId)
      ?.runners,
  ).toEqual([
    expect.objectContaining({ state: RunnerState.CREATING, count: 1 }),
    expect.objectContaining({ state: RunnerState.PREPARING, count: 1 }),
    expect.objectContaining({ state: RunnerState.WARM, count: 2 }),
    expect.objectContaining({ state: RunnerState.WAITING_FOR_WORK, count: 1 }),
    expect.objectContaining({ state: RunnerState.BUSY, count: 1 }),
    expect.objectContaining({ state: RunnerState.DELETING, count: 1 }),
  ]);
});

test.each([
  [MOCK_IDS.globex, "the organization has no OwnSand link"],
  [
    MOCK_IDS.northwind,
    "the OwnSand link is invalid; an admin must set a new OwnSand API key",
  ],
])(
  "capacity rejects an inactive OwnSand link in %s",
  async (organizationId, rawMessage) => {
    const work = createClient(WorkService, createMockTransport());
    await expect(work.getCapacity({ organizationId })).rejects.toMatchObject({
      code: Code.FailedPrecondition,
      rawMessage,
    });
  },
);

const missingOwnSandLink = {
  code: Code.NotFound,
  rawMessage: "OwnSand link not found",
};

test("removing an absent OwnSand link leaves work and audit history unchanged", async () => {
  const request = { organizationId: MOCK_IDS.globex };
  const listWork = { ...request, includeEnded: true };
  const transport = createMockTransport({
    workFixtures: [
      {
        ...request,
        requests: [{ requestId: "queued-job", state: RequestState.QUEUED }],
        runners: [{ runnerId: "warm-runner", state: RunnerState.WARM }],
      },
    ],
  });
  const link = createClient(OwnSandLinkService, transport);
  const work = createClient(WorkService, transport);
  const audit = createClient(AuditService, transport);
  const requests = structuredClone(await work.listRequests(listWork));
  const runners = structuredClone(await work.listRunners(listWork));
  const events = structuredClone(await audit.listAuditEvents(request));

  await expect(link.removeOwnSandLink(request)).rejects.toMatchObject(
    missingOwnSandLink,
  );

  expect(await work.listRequests(listWork)).toEqual(requests);
  expect(await work.listRunners(listWork)).toEqual(runners);
  expect(await audit.listAuditEvents(request)).toEqual(events);
  expect((await link.getOwnSandLink(request)).link).toBeUndefined();
});

test("removing an absent OwnSand link checks membership and role first", async () => {
  const transport = createMockTransport();
  const link = createClient(OwnSandLinkService, transport);
  const members = createClient(MemberService, transport);
  const request = { organizationId: MOCK_IDS.contoso };

  await expect(link.removeOwnSandLink(request)).rejects.toMatchObject({
    code: Code.NotFound,
    rawMessage: "organization not found",
  });

  await members.acceptInvitation({ invitationSecret: MOCK_INVITATION_SECRET });
  await expect(link.removeOwnSandLink(request)).rejects.toMatchObject({
    code: Code.PermissionDenied,
    rawMessage: "needs the admin role",
  });
});

test("removing OwnSand ends each active request and publishes its terminal outcome once", async () => {
  const organizationId = MOCK_IDS.acme;
  const otherOrgId = MOCK_IDS.globex;
  const devSessionId = "running-dev";
  const agentSessionId = "running-agent";
  const unboundId = "starting-without-runner";
  const deletingAgentId = "agent-with-deleting-runner";
  const transport = createMockTransport({
    watchQueueEndsAfterMs: 500,
    workFixtures: [
      {
        organizationId,
        requests: [
          {
            requestId: devSessionId,
            kind: WorkloadKind.DEV_SESSION,
            state: RequestState.RUNNING,
            runnerId: "dev-runner",
            ownerUserId: MOCK_USER,
          },
          {
            requestId: agentSessionId,
            kind: WorkloadKind.AGENT_SESSION,
            state: RequestState.RUNNING,
            runnerId: "agent-runner",
          },
          {
            requestId: unboundId,
            kind: WorkloadKind.AGENT_SESSION,
            state: RequestState.STARTING,
          },
          {
            requestId: deletingAgentId,
            kind: WorkloadKind.AGENT_SESSION,
            state: RequestState.RUNNING,
            runnerId: "deleting-runner",
          },
        ],
        runners: [
          {
            runnerId: "dev-runner",
            requestId: devSessionId,
            kind: WorkloadKind.DEV_SESSION,
            state: RunnerState.BUSY,
          },
          {
            runnerId: "agent-runner",
            requestId: agentSessionId,
            kind: WorkloadKind.AGENT_SESSION,
            state: RunnerState.BUSY,
          },
          {
            runnerId: "deleting-runner",
            requestId: deletingAgentId,
            kind: WorkloadKind.AGENT_SESSION,
            state: RunnerState.DELETING,
          },
        ],
        devSessions: [
          {
            devSessionId,
            ownerUserId: MOCK_USER,
            runnerId: "dev-runner",
            state: RequestState.RUNNING,
          },
        ],
      },
      {
        organizationId: otherOrgId,
        requests: [
          {
            requestId: "other-org-job",
            kind: WorkloadKind.GITHUB_JOB,
            state: RequestState.RUNNING,
          },
        ],
      },
    ],
  });
  const link = createClient(OwnSandLinkService, transport);
  const work = createClient(WorkService, transport);
  const sessions = createClient(DevSessionService, transport);
  const audit = createClient(AuditService, transport);
  const endedJob = structuredClone(
    (await work.getRequest({ organizationId, requestId: MOCK_IDS.endedJob }))
      .request,
  );
  const otherOrgRequests = { organizationId: otherOrgId, includeEnded: true };
  const otherHistory = structuredClone(
    await work.listRequests(otherOrgRequests),
  );
  expect(otherHistory.requests).toHaveLength(1);
  expect((await link.getOwnSandLink({ organizationId })).link).toBeDefined();
  const live = new AbortController();
  const fresh = new AbortController();
  try {
    const watch = work
      .watchQueue({ organizationId }, { signal: live.signal, timeoutMs: 5_000 })
      [Symbol.asyncIterator]();
    const initialRequests: Request[] = [];
    for (;;) {
      const next = await watch.next();
      if (next.done) throw new Error("Expected a complete queue snapshot");
      const event = next.value.event;
      if (event.case === "snapshotComplete") break;
      if (event.case === "request") initialRequests.push(event.value);
    }
    expect(initialRequests).toContainEqual(
      expect.objectContaining({
        requestId: MOCK_IDS.runningJob,
        state: RequestState.RUNNING,
      }),
    );

    const removalStartedAt = Date.now();
    await link.removeOwnSandLink({ organizationId });
    const removalFinishedAt = Date.now();

    const updates: Request[] = [];
    for (let next = await watch.next(); !next.done; next = await watch.next()) {
      if (next.value.event.case === "request")
        updates.push(next.value.event.value);
    }
    const history = (
      await work.listRequests({ organizationId, includeEnded: true })
    ).requests;
    const byId = new Map(
      history.map((request) => [request.requestId, request]),
    );
    expect(updates.map((request) => request.requestId).sort()).toEqual(
      initialRequests.map((request) => request.requestId).sort(),
    );
    expect(updates).toHaveLength(8);
    for (const request of updates) {
      expect(request).toEqual(byId.get(request.requestId));
      expect(request.endedAt).toBeDefined();
      const endedAt = timestampMs(request.endedAt!);
      expect(endedAt).toBeGreaterThanOrEqual(removalStartedAt);
      expect(endedAt).toBeLessThanOrEqual(removalFinishedAt);
    }
    for (const requestId of [
      MOCK_IDS.queuedJob,
      MOCK_IDS.queuedAgent,
      MOCK_IDS.queuedDev,
    ]) {
      expect(byId.get(requestId)).toMatchObject({
        state: RequestState.CANCELED,
        endReason: RequestEndReason.INTEGRATION_REMOVED,
      });
    }
    for (const requestId of [MOCK_IDS.runningJob, unboundId, deletingAgentId]) {
      expect(byId.get(requestId)).toMatchObject({
        state: RequestState.FAILED,
        endReason: RequestEndReason.INTEGRATION_REMOVED,
      });
    }
    expect(byId.get(devSessionId)).toMatchObject({
      state: RequestState.ENDED,
      endReason: RequestEndReason.SESSION_DELETED,
    });
    expect(byId.get(agentSessionId)).toMatchObject({
      state: RequestState.FAILED,
      endReason: RequestEndReason.RUNNER_LOST,
    });
    for (const id of [MOCK_IDS.queuedDev, devSessionId]) {
      const request = byId.get(id)!;
      const { devSession } = await sessions.getDevSession({
        organizationId,
        devSessionId: id,
      });
      expect(devSession).toMatchObject({
        state: request.state,
        endReason: request.endReason,
        endedAt: request.endedAt,
      });
    }
    expect(byId.get(MOCK_IDS.endedJob)).toEqual(endedJob);
    expect(await work.listRequests(otherOrgRequests)).toEqual(otherHistory);
    expect((await work.listRequests({ organizationId })).requests).toEqual([]);
    expect(
      (await link.getOwnSandLink({ organizationId })).link,
    ).toBeUndefined();
    const { runner } = await work.getRunner({
      organizationId,
      runnerId: MOCK_IDS.busyRunner,
    });
    expect(runner).toMatchObject({
      state: RunnerState.DELETED,
      endReason: RunnerEndReason.CLEAN_UP,
    });
    const events = (await audit.listAuditEvents({ organizationId })).events;
    expect(
      events.filter(
        (event) => event.action === AuditAction.OWNSAND_LINK_REMOVED,
      ),
    ).toMatchObject([
      {
        action: AuditAction.OWNSAND_LINK_REMOVED,
        targetKind: AuditTargetKind.OWNSAND_LINK,
        targetId: organizationId,
      },
    ]);
    await expect(
      link.removeOwnSandLink({ organizationId }),
    ).rejects.toMatchObject(missingOwnSandLink);
    expect((await audit.listAuditEvents({ organizationId })).events).toEqual(
      events,
    );

    const snapshot = work
      .watchQueue(
        { organizationId },
        { signal: fresh.signal, timeoutMs: 5_000 },
      )
      [Symbol.asyncIterator]();
    await expect(snapshot.next()).resolves.toMatchObject({
      done: false,
      value: { event: { case: "snapshotComplete" } },
    });
    await snapshot.return?.();
  } finally {
    live.abort();
    fresh.abort();
  }
});

test.each([
  [MOCK_IDS.northwind, MOCK_IDS.deletedRunner, Code.PermissionDenied],
  [MOCK_IDS.acme, MOCK_IDS.queuedJob, Code.NotFound],
])(
  "delete runner in %s returns the existing error code",
  async (organizationId, runnerId, code) => {
    const work = createClient(WorkService, createMockTransport());
    await expect(
      work.deleteRunner({ organizationId, runnerId }),
    ).rejects.toMatchObject({
      code,
    });
  },
);

test("GitHub installation state is scoped to its organization and can be used once before expiry", async () => {
  const issuedAt = Date.UTC(2026, 0, 1) + 123;
  const clock = rs.spyOn(Date, "now").mockReturnValue(issuedAt);
  try {
    const github = createClient(GitHubService, createMockTransport());
    const organizationId = MOCK_IDS.globex;
    const { url, expireTime } = await github.createGitHubInstallUrl({
      organizationId,
    });
    const state = new URL(url).searchParams.get("state");
    if (!state || !expireTime) throw new Error("Expected state and expiry");
    expect(timestampMs(expireTime)).toBe(issuedAt + 600_000);
    clock.mockReturnValue(timestampMs(expireTime) - 1);

    const callback = {
      organizationId,
      githubInstallationId: 51_000_002n,
      code: "fixture-code",
      state,
    };
    await expect(
      github.linkGitHubInstallation({
        ...callback,
        organizationId: MOCK_IDS.acme,
      }),
    ).rejects.toMatchObject(invalidInstallationState);
    await expect(
      github.linkGitHubInstallation({ ...callback, state: "unknown-state" }),
    ).rejects.toMatchObject(invalidInstallationState);
    await expect(
      github.linkGitHubInstallation(callback),
    ).resolves.toMatchObject({
      installation: {
        organizationId,
        githubInstallationId: callback.githubInstallationId,
      },
    });
    await expect(
      github.linkGitHubInstallation({
        ...callback,
        githubInstallationId: 51_000_003n,
      }),
    ).rejects.toMatchObject(invalidInstallationState);
  } finally {
    clock.mockRestore();
  }
});

test.each([0, 1])(
  "GitHub installation state is rejected %i ms after its expiry without creating an installation or audit event",
  async (offsetMs) => {
    const clock = rs
      .spyOn(Date, "now")
      .mockReturnValue(Date.UTC(2026, 0, 1) + 123);
    try {
      const transport = createMockTransport();
      const github = createClient(GitHubService, transport);
      const audit = createClient(AuditService, transport);
      const organizationId = MOCK_IDS.globex;
      const { url, expireTime } = await github.createGitHubInstallUrl({
        organizationId,
      });
      const state = new URL(url).searchParams.get("state");
      if (!state || !expireTime) throw new Error("Expected state and expiry");
      const { events } = await audit.listAuditEvents({ organizationId });
      clock.mockReturnValue(timestampMs(expireTime) + offsetMs);

      await expect(
        github.linkGitHubInstallation({
          organizationId,
          githubInstallationId: 51_000_002n,
          code: "fixture-code",
          state,
        }),
      ).rejects.toMatchObject(invalidInstallationState);
      expect(
        (await github.listGitHubInstallations({ organizationId }))
          .installations,
      ).toEqual([]);
      expect((await audit.listAuditEvents({ organizationId })).events).toEqual(
        events,
      );
    } finally {
      clock.mockRestore();
    }
  },
);

test("a cross-organization GitHub installation conflict consumes its issued state without changing either organization", async () => {
  const transport = createMockTransport();
  const github = createClient(GitHubService, transport);
  const audit = createClient(AuditService, transport);
  const source = { organizationId: MOCK_IDS.acme };
  const destination = { organizationId: MOCK_IDS.globex };
  const original = await github.listGitHubInstallations(source);
  expect(original.installations[0]).toMatchObject({
    githubInstallationId: 41_000_001n,
  });
  const auditBefore = await audit.listAuditEvents(destination);
  const { url } = await github.createGitHubInstallUrl(destination);
  const state = new URL(url).searchParams.get("state");
  if (!state) throw new Error("Expected an issued installation state");
  const callback = { ...destination, code: "fixture-code", state };

  await expect(
    github.linkGitHubInstallation({
      ...callback,
      githubInstallationId: 41_000_001n,
    }),
  ).rejects.toMatchObject({ code: Code.AlreadyExists });
  await expect(
    github.linkGitHubInstallation({
      ...callback,
      githubInstallationId: 51_000_002n,
    }),
  ).rejects.toMatchObject(invalidInstallationState);
  expect(
    (await github.listGitHubInstallations(destination)).installations,
  ).toEqual([]);
  expect(await github.listGitHubInstallations(source)).toEqual(original);
  expect(await audit.listAuditEvents(destination)).toEqual(auditBefore);
});

test("a GitHub installation linked in the mock can move to another organization after unlink", async () => {
  const transport = createMockTransport();
  const github = createClient(GitHubService, transport);
  const audit = createClient(AuditService, transport);
  const githubInstallationId = 51_000_002n;
  const link = async (organizationId: string) => {
    const { url } = await github.createGitHubInstallUrl({ organizationId });
    const state = new URL(url).searchParams.get("state");
    if (!state) throw new Error("Expected an issued installation state");
    return github.linkGitHubInstallation({
      organizationId,
      githubInstallationId,
      code: "fixture-code",
      state,
    });
  };
  const { installation } = await link(MOCK_IDS.acme);
  if (!installation) throw new Error("Expected the linked installation");

  for (const organizationId of [MOCK_IDS.acme, MOCK_IDS.globex]) {
    const request = { organizationId };
    const installationsBefore = await github.listGitHubInstallations(request);
    const auditBefore = await audit.listAuditEvents(request);
    await expect(link(organizationId)).rejects.toMatchObject({
      code: Code.AlreadyExists,
    });
    expect(await github.listGitHubInstallations(request)).toEqual(
      installationsBefore,
    );
    expect(await audit.listAuditEvents(request)).toEqual(auditBefore);
  }

  await github.unlinkGitHubInstallation({
    organizationId: MOCK_IDS.acme,
    installationId: installation.installationId,
  });
  await expect(link(MOCK_IDS.globex)).resolves.toMatchObject({
    installation: { organizationId: MOCK_IDS.globex, githubInstallationId },
  });
  await expect(
    github.linkGitHubInstallation({
      organizationId: MOCK_IDS.acme,
      githubInstallationId: 51_000_003n,
      code: "fixture-code",
      state: "mock-state",
    }),
  ).resolves.toMatchObject({
    installation: {
      organizationId: MOCK_IDS.acme,
      githubInstallationId: 51_000_003n,
    },
  });
});

test("unlinking GitHub ends the installation's warm runner and keeps its history", async () => {
  const transport = createMockTransport();
  const github = createClient(GitHubService, transport);
  const work = createClient(WorkService, transport);
  const audit = createClient(AuditService, transport);
  const org = { organizationId: MOCK_IDS.acme };
  const history = { ...org, includeEnded: true };
  const before = structuredClone((await work.listRunners(history)).runners);

  expect((await work.listRunners(org)).runners).toContainEqual(
    expect.objectContaining({
      runnerId: MOCK_IDS.warmRunner,
      state: RunnerState.WARM,
    }),
  );
  await github.unlinkGitHubInstallation({
    ...org,
    installationId: MOCK_IDS.installation,
  });

  const preserved = before.filter((r) => r.runnerId !== MOCK_IDS.warmRunner);
  expect((await work.listRunners(org)).runners).toEqual(
    preserved.filter((r) => r.runnerId === MOCK_IDS.busyRunner),
  );
  const { runners } = await work.listRunners(history);
  expect(runners.filter((r) => r.runnerId !== MOCK_IDS.warmRunner)).toEqual(
    preserved,
  );
  expect(runners.find((r) => r.runnerId === MOCK_IDS.warmRunner)).toMatchObject(
    {
      state: RunnerState.DELETED,
      endReason: RunnerEndReason.CLEAN_UP,
      endedAt: expect.any(Object),
    },
  );
  expect(
    (await github.listGitHubInstallations(org)).installations,
  ).not.toContainEqual(
    expect.objectContaining({ installationId: MOCK_IDS.installation }),
  );
  expect(
    (await work.getRequest({ ...org, requestId: MOCK_IDS.queuedJob })).request,
  ).toMatchObject({
    state: RequestState.CANCELED,
    endReason: RequestEndReason.INTEGRATION_REMOVED,
  });
  expect((await audit.listAuditEvents(org)).events).toContainEqual(
    expect.objectContaining({
      action: AuditAction.GITHUB_INSTALLATION_UNLINKED,
      targetKind: AuditTargetKind.GITHUB_INSTALLATION,
      targetId: MOCK_IDS.installation,
    }),
  );
});

test("unlinking a second GitHub installation leaves the original warm runner active", async () => {
  const transport = createMockTransport();
  const github = createClient(GitHubService, transport);
  const work = createClient(WorkService, transport);
  const org = { organizationId: MOCK_IDS.acme };
  const { installation } = await github.linkGitHubInstallation({
    ...org,
    githubInstallationId: 51_000_099n,
    code: "fixture-code",
    state: "mock-state",
  });
  if (!installation) throw new Error("Expected the second installation");

  await github.unlinkGitHubInstallation({
    ...org,
    installationId: installation.installationId,
  });

  expect((await work.listRunners(org)).runners).toContainEqual(
    expect.objectContaining({
      runnerId: MOCK_IDS.warmRunner,
      state: RunnerState.WARM,
    }),
  );
});

async function readSnapshotRunnerIds(
  stream: AsyncIterator<WatchQueueResponse, unknown>,
) {
  const ids = new Set<string>();
  for (;;) {
    const { done, value: message } = await stream.next();
    if (done) throw new Error("Expected a complete queue snapshot");
    if (message.event.case === "snapshotComplete") return ids;
    if (message.event.case === "runner") ids.add(message.event.value.runnerId);
  }
}

test("GitHub unlink sends a terminal runner event and omits the runner from fresh queue snapshots", async () => {
  const transport = createMockTransport();
  const github = createClient(GitHubService, transport);
  const work = createClient(WorkService, transport);
  const org = { organizationId: MOCK_IDS.acme };
  const stream: AsyncIterator<WatchQueueResponse, unknown> = work
    .watchQueue(org, { timeoutMs: 2000 })
    [Symbol.asyncIterator]();
  try {
    expect(await readSnapshotRunnerIds(stream)).toContain(MOCK_IDS.warmRunner);
    await github.unlinkGitHubInstallation({
      ...org,
      installationId: MOCK_IDS.installation,
    });
    for (;;) {
      const { done, value: message } = await stream.next();
      if (done) throw new Error("Expected a runner event after GitHub unlink");
      if (
        message.event.case === "runner" &&
        message.event.value.runnerId === MOCK_IDS.warmRunner
      ) {
        expect(message.event.value).toMatchObject({
          state: RunnerState.DELETED,
          endReason: RunnerEndReason.CLEAN_UP,
          endedAt: expect.any(Object),
        });
        break;
      }
    }
  } finally {
    await stream.return?.();
  }

  const fresh = work
    .watchQueue(org, { timeoutMs: 2000 })
    [Symbol.asyncIterator]();
  try {
    expect(await readSnapshotRunnerIds(fresh)).toEqual(
      new Set([MOCK_IDS.busyRunner]),
    );
  } finally {
    await fresh.return?.();
  }
});

test("an agent integration name conflict preserves the integration list and audit history", async () => {
  const transport = createMockTransport();
  const integrations = createClient(AgentIntegrationService, transport);
  const audit = createClient(AuditService, transport);
  const organizationId = MOCK_IDS.acme;
  const id = { organizationId, agentIntegrationId: MOCK_IDS.claudeIntegration };
  const listBefore = structuredClone(
    await integrations.listAgentIntegrations({ organizationId }),
  );
  const integrationBefore = structuredClone(
    await integrations.getAgentIntegration(id),
  );
  const auditBefore = structuredClone(
    await audit.listAuditEvents({ organizationId }),
  );
  const conflict = {
    code: Code.AlreadyExists,
    rawMessage: "an agent integration with this name exists",
  };

  await expect(
    integrations.updateAgentIntegration({
      ...id,
      name: " codex-prod ",
      updateMask: { paths: ["name"] },
    }),
  ).rejects.toMatchObject(conflict);
  await expect(
    integrations.createAgentIntegration({
      organizationId,
      vendor: AgentVendor.CLAUDE_CODE,
      name: "codex-prod",
    }),
  ).rejects.toMatchObject(conflict);

  expect(await integrations.getAgentIntegration(id)).toEqual(integrationBefore);
  expect(await integrations.listAgentIntegrations({ organizationId })).toEqual(
    listBefore,
  );
  expect(await audit.listAuditEvents({ organizationId })).toEqual(auditBefore);
});

test("creating an integration without credentials preserves the integration list and audit history", async () => {
  const transport = createMockTransport();
  const integrations = createClient(AgentIntegrationService, transport);
  const audit = createClient(AuditService, transport);
  const organizationId = MOCK_IDS.acme;
  const listBefore = structuredClone(
    await integrations.listAgentIntegrations({ organizationId }),
  );
  const auditBefore = structuredClone(
    await audit.listAuditEvents({ organizationId }),
  );

  await expect(
    integrations.createAgentIntegration({
      organizationId,
      vendor: AgentVendor.CLAUDE_CODE,
      name: "claude-without-credentials",
    }),
  ).rejects.toMatchObject({
    code: Code.InvalidArgument,
    rawMessage: "credentials are required",
  });
  expect(await integrations.listAgentIntegrations({ organizationId })).toEqual(
    listBefore,
  );
  expect(await audit.listAuditEvents({ organizationId })).toEqual(auditBefore);
});

test.each(["claude-team", "claude-renamed"])(
  "an integration can update its name to %s",
  async (name) => {
    const transport = createMockTransport();
    const integrations = createClient(AgentIntegrationService, transport);
    const audit = createClient(AuditService, transport);
    const organizationId = MOCK_IDS.acme;
    const id = {
      organizationId,
      agentIntegrationId: MOCK_IDS.claudeIntegration,
    };
    const before = structuredClone(await integrations.getAgentIntegration(id));
    const auditBefore = structuredClone(
      await audit.listAuditEvents({ organizationId }),
    );
    const { integration } = await integrations.updateAgentIntegration({
      ...id,
      name,
      updateMask: { paths: ["name"] },
    });

    expect(integration).toMatchObject({
      name,
      credentials: before.integration?.credentials,
    });
    expect((await integrations.getAgentIntegration(id)).integration).toEqual(
      integration,
    );
    const {
      events: [updated, ...previousEvents],
    } = await audit.listAuditEvents({ organizationId });
    expect(updated).toMatchObject({
      action: AuditAction.AGENT_INTEGRATION_UPDATED,
      targetId: id.agentIntegrationId,
    });
    expect(previousEvents).toEqual(auditBefore.events);
  },
);

test("a rejected combined update preserves the integration and its audit history", async () => {
  const createdAt = Date.UTC(2026, 0, 1);
  const updatedAt = Date.UTC(2026, 0, 2);
  const clock = rs.spyOn(Date, "now").mockReturnValue(createdAt);
  try {
    const transport = createMockTransport();
    const integrations = createClient(AgentIntegrationService, transport);
    const audit = createClient(AuditService, transport);
    const organizationId = MOCK_IDS.acme;
    const { integration } = await integrations.createAgentIntegration({
      organizationId,
      vendor: AgentVendor.CURSOR,
      name: "cursor-before",
      settings: { case: "cursor", value: { poolName: "pool-before" } },
      credentials: {
        case: "cursorCredentials",
        value: { serviceAccountApiKey: "synthetic-cursor-before" },
      },
    });
    if (integration === undefined) throw new Error("Expected an integration");
    const id = {
      organizationId,
      agentIntegrationId: integration.agentIntegrationId,
    };
    const before = structuredClone(await integrations.getAgentIntegration(id));
    const auditBefore = structuredClone(
      await audit.listAuditEvents({ organizationId }),
    );
    const update = {
      ...id,
      name: " cursor-after ",
      settings: { case: "cursor" as const, value: { poolName: "pool-after" } },
      updateMask: { paths: ["name", "settings", "credentials"] },
    };

    clock.mockReturnValue(updatedAt);
    await expect(
      integrations.updateAgentIntegration(update),
    ).rejects.toMatchObject({
      code: Code.InvalidArgument,
      rawMessage: "credentials are required",
    });
    expect(await integrations.getAgentIntegration(id)).toEqual(before);
    expect(await audit.listAuditEvents({ organizationId })).toEqual(
      auditBefore,
    );

    await expect(
      integrations.updateAgentIntegration({
        ...update,
        credentials: {
          case: "claudeCodeCredentials",
          value: { environmentKey: "synthetic-claude" },
        },
      }),
    ).rejects.toMatchObject({
      code: Code.InvalidArgument,
      rawMessage: "credentials do not match the vendor",
    });
    expect(await integrations.getAgentIntegration(id)).toEqual(before);
    expect(await audit.listAuditEvents({ organizationId })).toEqual(
      auditBefore,
    );

    for (const edit of [
      update,
      { ...id, updateMask: { paths: ["credentials"] } },
    ]) {
      await expect(
        integrations.updateAgentIntegration({
          ...edit,
          credentials: { case: "cursorCredentials", value: {} },
        }),
      ).rejects.toMatchObject({
        code: Code.InvalidArgument,
        rawMessage:
          "invalid credentials: set at least one credential to replace",
      });
      expect(await integrations.getAgentIntegration(id)).toEqual(before);
      expect(await audit.listAuditEvents({ organizationId })).toEqual(
        auditBefore,
      );
    }

    const saved = await integrations.updateAgentIntegration({
      ...update,
      credentials: {
        case: "cursorCredentials",
        value: { serviceAccountApiKey: "synthetic-cursor-after" },
      },
    });
    const updateTime = timestampFromMs(updatedAt);
    expect(saved.integration).toMatchObject({
      name: "cursor-after",
      settings: update.settings,
      credentials: [
        {
          credential: AgentCredential.SERVICE_ACCOUNT_API_KEY,
          credentialSet: true,
          credentialSetAt: updateTime,
        },
      ],
      updateTime,
    });
    expect((await integrations.getAgentIntegration(id)).integration).toEqual(
      saved.integration,
    );
    const { events } = await audit.listAuditEvents({ organizationId });
    const [updated, replaced, ...previousEvents] = events;
    const event = {
      targetKind: AuditTargetKind.AGENT_INTEGRATION,
      targetId: id.agentIntegrationId,
      createTime: updateTime,
    };
    expect(updated).toMatchObject({
      ...event,
      action: AuditAction.AGENT_INTEGRATION_UPDATED,
    });
    expect(replaced).toMatchObject({
      ...event,
      action: AuditAction.AGENT_INTEGRATION_CREDENTIALS_REPLACED,
    });
    expect(previousEvents).toEqual(auditBefore.events);
  } finally {
    clock.mockRestore();
  }
});

test("a partial credential replacement preserves omitted credential metadata", async () => {
  const createdAt = Date.UTC(2026, 0, 1);
  const replacementAt = Date.UTC(2026, 0, 2, 0, 0, 0, 123);
  const clock = rs.spyOn(Date, "now").mockReturnValue(createdAt);
  try {
    const transport = createMockTransport();
    const integrations = createClient(AgentIntegrationService, transport);
    const audit = createClient(AuditService, transport);
    const organizationId = MOCK_IDS.acme;
    const id = {
      organizationId,
      agentIntegrationId: MOCK_IDS.codexIntegration,
    };
    const { integration: before } = structuredClone(
      await integrations.getAgentIntegration(id),
    );
    if (before === undefined) throw new Error("Expected an integration");

    clock.mockReturnValue(replacementAt);
    const { integration } = await integrations.updateAgentIntegration({
      ...id,
      credentials: {
        case: "openaiCodexCredentials",
        value: { apiKey: "synthetic-codex-replacement" },
      },
      updateMask: { paths: ["credentials"] },
    });

    const replacementTime = timestampFromMs(replacementAt);
    expect(integration).toEqual({
      ...before,
      updateTime: replacementTime,
      credentials: before.credentials.map((state) =>
        state.credential === AgentCredential.API_KEY
          ? { ...state, credentialSetAt: replacementTime }
          : state,
      ),
    });
    expect((await integrations.getAgentIntegration(id)).integration).toEqual(
      integration,
    );
    const { events } = await audit.listAuditEvents({ organizationId });
    const replacements = events.filter(
      (event) =>
        event.action === AuditAction.AGENT_INTEGRATION_CREDENTIALS_REPLACED,
    );
    expect(replacements).toEqual([
      expect.objectContaining({
        targetId: id.agentIntegrationId,
        createTime: replacementTime,
      }),
    ]);
  } finally {
    clock.mockRestore();
  }
});
