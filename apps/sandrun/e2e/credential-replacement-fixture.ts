// SPDX-License-Identifier: Apache-2.0
import { equals } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  AgentCredential,
  AgentIntegrationService,
  GetAgentIntegrationResponseSchema,
} from "@delino/sandrun-protos/v1/agent_integration_pb";
import {
  AuditAction,
  AuditService,
  ListAuditEventsResponseSchema,
} from "@delino/sandrun-protos/v1/audit_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

async function checkCredentialReplacement() {
  const originalNow = Date.now;
  let now = Date.UTC(2026, 0, 1);
  Date.now = () => now;
  try {
    const transport = createMockTransport();
    const integrations = createClient(AgentIntegrationService, transport);
    const audit = createClient(AuditService, transport);
    const org = { organizationId: MOCK_IDS.acme };
    const id = { ...org, agentIntegrationId: MOCK_IDS.codexIntegration };
    const read = () => integrations.getAgentIntegration(id);
    const history = () => audit.listAuditEvents(org);
    const before = structuredClone(await read());
    const auditBefore = structuredClone(await history());
    if (before.integration === undefined)
      throw new Error("Missing fixture integration");

    now += 1_234;
    const error = await integrations
      .updateAgentIntegration({
        ...id,
        name: "codex-renamed",
        settings: { case: "openaiCodex", value: {} },
        credentials: { case: "openaiCodexCredentials", value: {} },
        updateMask: { paths: ["name", "settings", "credentials"] },
      })
      .then(
        () => undefined,
        (cause: unknown) => ConnectError.from(cause),
      );
    const emptyRejected =
      error?.code === Code.InvalidArgument &&
      error.rawMessage ===
        "invalid credentials: set at least one credential to replace";
    const unchangedIntegration = equals(
      GetAgentIntegrationResponseSchema,
      await read(),
      before,
    );
    const unchangedAudit = equals(
      ListAuditEventsResponseSchema,
      await history(),
      auditBefore,
    );

    now += 1_234;
    const rotationTime = timestampFromMs(now);
    await integrations.updateAgentIntegration({
      ...id,
      credentials: {
        case: "openaiCodexCredentials",
        value: { apiKey: "synthetic-api-replacement" },
      },
      updateMask: { paths: ["credentials"] },
    });
    const expected = {
      ...before,
      integration: {
        ...before.integration,
        updateTime: rotationTime,
        credentials: before.integration.credentials.map((state) =>
          state.credential === AgentCredential.API_KEY
            ? { ...state, credentialSetAt: rotationTime }
            : state,
        ),
      },
    };
    const replacementExact = equals(
      GetAgentIntegrationResponseSchema,
      await read(),
      expected,
    );
    const beforeRename = structuredClone(await history());
    const rotationEvents = beforeRename.events.filter(
      (event) =>
        event.action === AuditAction.AGENT_INTEGRATION_CREDENTIALS_REPLACED,
    );
    const replacementAudited =
      rotationEvents.length === 1 &&
      rotationEvents[0]?.targetId === id.agentIntegrationId &&
      rotationEvents[0]?.createTime?.seconds === rotationTime.seconds &&
      rotationEvents[0]?.createTime?.nanos === rotationTime.nanos;

    now += 1_234;
    const name = "codex-name-only";
    await integrations.updateAgentIntegration({
      ...id,
      name,
      updateMask: { paths: ["name"] },
    });
    const nameOnlyPreserved = equals(
      GetAgentIntegrationResponseSchema,
      await read(),
      {
        ...expected,
        integration: {
          ...expected.integration,
          name,
          updateTime: timestampFromMs(now),
        },
      },
    );
    const afterRename = await history();
    const [updated, ...events] = afterRename.events;
    const nameOnlyAudited =
      updated?.action === AuditAction.AGENT_INTEGRATION_UPDATED &&
      updated.targetId === id.agentIntegrationId &&
      equals(
        ListAuditEventsResponseSchema,
        { ...afterRename, events },
        beforeRename,
      );

    return {
      emptyRejected,
      unchangedIntegration,
      unchangedAudit,
      replacementExact,
      replacementAudited,
      nameOnlyPreserved,
      nameOnlyAudited,
    };
  } finally {
    Date.now = originalNow;
  }
}

const output = document.createElement("output");
document.body.append(output);
void checkCredentialReplacement().then(
  (result) => {
    output.textContent = JSON.stringify(result);
  },
  () => {
    output.textContent = "Credential replacement fixture failed";
  },
);
