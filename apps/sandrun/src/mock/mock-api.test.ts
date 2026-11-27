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
  RunnerEndReason,
  RunnerState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/sandrun-protos/v1/runner_profile_pb";
import { type Request, WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { expect, rs, test } from "@rstest/core";
import { createMockTransport, MOCK_IDS, MOCK_USER } from "./mock-api.ts";

const invalidInstallationState = {
  code: Code.FailedPrecondition,
  rawMessage: "the installation state is not valid or has expired",
};

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

const apiKeyClock = Date.UTC(2026, 0, 1) + 250;
const apiKeyClockTimestamp = timestampFromMs(apiKeyClock);

const linuxProfile = {
  organizationId: MOCK_IDS.acme,
  runnerProfileId: MOCK_IDS.linuxProfile,
};
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
    expect(
      (await audit.listAuditEvents({ organizationId })).events[0],
    ).toMatchObject({
      action: AuditAction.OWNSAND_LINK_REMOVED,
      targetKind: AuditTargetKind.OWNSAND_LINK,
      targetId: organizationId,
    });

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
    const integrations = createClient(
      AgentIntegrationService,
      createMockTransport(),
    );
    const id = {
      organizationId: MOCK_IDS.acme,
      agentIntegrationId: MOCK_IDS.claudeIntegration,
    };
    const before = structuredClone(await integrations.getAgentIntegration(id));
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
