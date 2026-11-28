// SPDX-License-Identifier: Apache-2.0
import { create, isMessage } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import {
  AgentIntegrationService,
  ListAgentIntegrationsRequestSchema,
  ListAgentIntegrationsResponseSchema,
} from "@delino/sandrun-protos/v1/agent_integration_pb";
import {
  type UpdateRunnerProfileRequest,
  UpdateRunnerProfileRequestSchema,
} from "@delino/sandrun-protos/v1/runner_profile_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const PROFILE_INTEGRATION_FIXTURE = "sandrun.profileIntegrationFixture";

export function pagedProfileIntegrations(
  base: Transport,
  mode: "codex" | "claude" | "missing",
) {
  const integrations = createClient(AgentIntegrationService, base);
  const updates: UpdateRunnerProfileRequest[] = [];
  const pageTokens: string[] = [];
  const continuations: Array<(fail?: boolean) => void> = [];
  const transport: Transport = {
    ...base,
    async unary(method, ...args) {
      const request = create(method.input, args[3]);
      if (isMessage(request, UpdateRunnerProfileRequestSchema)) {
        updates.push(request);
      }
      const response = await base.unary(method, ...args);
      if (
        isMessage(request, ListAgentIntegrationsRequestSchema) &&
        isMessage(response.message, ListAgentIntegrationsResponseSchema)
      ) {
        // A Strict Mode remount can cancel a request before the fixture sees it.
        args[0]?.throwIfAborted();
        const token = request.pageToken;
        pageTokens.push(token);
        if (token !== "") {
          const fail = await new Promise<boolean | undefined>((release) =>
            continuations.push(release),
          );
          if (fail)
            throw new ConnectError("Continuation failed", Code.Internal);
        }
        if (mode === "codex") {
          // Swap the two rows without changing the persisted profiles.
          const { integration } = await integrations.getAgentIntegration({
            organizationId: request.organizationId,
            agentIntegrationId:
              token === ""
                ? MOCK_IDS.claudeIntegration
                : MOCK_IDS.codexIntegration,
          });
          response.message.integrations = [integration!];
        } else if (mode === "missing" && token !== "") {
          response.message.integrations = [];
        }
      }
      return response;
    },
  };
  return { transport, updates, pageTokens, continuations };
}
