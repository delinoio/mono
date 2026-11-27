// SPDX-License-Identifier: Apache-2.0
import { timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
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
import { AgentVendor, ApiKeyState } from "@delino/sandrun-protos/v1/common_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { expect, rs, test } from "@rstest/core";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";

const invalidInstallationState = {
  code: Code.FailedPrecondition,
  rawMessage: "the installation state is not valid or has expired",
};

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
