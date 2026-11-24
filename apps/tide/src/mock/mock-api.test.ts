// SPDX-License-Identifier: Apache-2.0
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, createClient } from "@connectrpc/connect";
import {
  AgentCredential,
  AgentIntegrationService,
} from "@delino/tide-protos/v1/agent_integration_pb";
import {
  AuditAction,
  AuditService,
  AuditTargetKind,
} from "@delino/tide-protos/v1/audit_pb";
import { AgentVendor } from "@delino/tide-protos/v1/common_pb";
import { expect, rs, test } from "@rstest/core";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";

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
