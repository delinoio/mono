// SPDX-License-Identifier: Apache-2.0
import { equals } from "@bufbuild/protobuf";
import { ConnectError, createClient } from "@connectrpc/connect";
import {
  AuditAction,
  AuditService,
  AuditTargetKind,
  ListAuditEventsResponseSchema,
} from "@delino/sandrun-protos/v1/audit_pb";
import {
  GetRunnerProfileResponseSchema,
  RunnerProfileService,
} from "@delino/sandrun-protos/v1/runner_profile_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

async function checkRunnerProfileUpdateMasks() {
  const transport = createMockTransport();
  const profiles = createClient(RunnerProfileService, transport);
  const audit = createClient(AuditService, transport);
  const org = { organizationId: MOCK_IDS.acme };
  const id = { ...org, runnerProfileId: MOCK_IDS.linuxProfile };
  const read = () => profiles.getRunnerProfile(id);
  const history = () => audit.listAuditEvents(org);
  const name = "linux-build-new";
  const request = { ...id, profile: { name } };

  async function checkRejection(mask: { updateMask?: { paths: string[] } }) {
    const before = structuredClone(await read());
    const auditBefore = structuredClone(await history());
    const error = await profiles
      .updateRunnerProfile({ ...request, ...mask })
      .then(
        () => undefined,
        (cause: unknown) => ConnectError.from(cause),
      );
    return {
      errorCode: error?.code ?? null,
      errorMessage: error?.rawMessage ?? null,
      unchangedProfile: equals(
        GetRunnerProfileResponseSchema,
        await read(),
        before,
      ),
      unchangedAudit: equals(
        ListAuditEventsResponseSchema,
        await history(),
        auditBefore,
      ),
    };
  }

  const omitted = await checkRejection({});
  const empty = await checkRejection({ updateMask: { paths: [] } });
  const auditBefore = structuredClone(await history());
  const { profile } = await profiles.updateRunnerProfile({
    ...request,
    updateMask: { paths: ["name"] },
  });
  const stored = await read();
  const after = await history();
  const [updated, ...events] = after.events;

  return {
    omitted,
    empty,
    name: stored.profile?.name,
    githubLabel: stored.profile?.githubLabel,
    responseMatchesStored: equals(
      GetRunnerProfileResponseSchema,
      { ...stored, profile },
      stored,
    ),
    addedOneUpdateEvent:
      updated?.action === AuditAction.RUNNER_PROFILE_UPDATED &&
      updated.targetKind === AuditTargetKind.RUNNER_PROFILE &&
      updated.targetId === id.runnerProfileId &&
      equals(ListAuditEventsResponseSchema, { ...after, events }, auditBefore),
  };
}

const output = document.createElement("output");
document.body.append(output);
void checkRunnerProfileUpdateMasks().then(
  (result) => {
    output.textContent = JSON.stringify(result);
  },
  () => {
    output.textContent = "Runner profile update mask fixture failed";
  },
);
