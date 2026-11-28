// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import type {
  ListGitHubRunnerGroupsResponse,
  UpdateGitHubInstallationRequest,
} from "@delino/sandrun-protos/v1/github_pb";

export function runnerGroupFixture(base: Transport) {
  let groupIds: readonly bigint[] | undefined;
  let failure: Code | undefined;
  let nextListGate: Promise<void> | undefined;
  const updates: UpdateGitHubInstallationRequest[] = [];

  const transport: Transport = {
    ...base,
    async unary(method, ...args) {
      if (method.name === "UpdateGitHubInstallation") {
        updates.push(args[3] as UpdateGitHubInstallationRequest);
      }
      if (method.name !== "ListGitHubRunnerGroups") {
        return base.unary(method, ...args);
      }
      const gate = nextListGate;
      nextListGate = undefined;
      if (gate) await gate;
      if (failure !== undefined) {
        throw new ConnectError("Runner group refresh failed", failure);
      }
      const response = await base.unary(method, ...args);
      const list = response.message as ListGitHubRunnerGroupsResponse;
      const ids = groupIds;
      if (ids !== undefined) {
        list.runnerGroups = list.runnerGroups.filter((group) =>
          ids.includes(group.githubRunnerGroupId),
        );
      }
      return response;
    },
  };
  return {
    transport,
    updates,
    setGroups(ids?: readonly bigint[]) {
      groupIds = ids;
    },
    fail(code?: Code) {
      failure = code;
    },
    holdNextList() {
      let release!: () => void;
      nextListGate = new Promise<void>((resolve) => {
        release = resolve;
      });
      return release;
    },
  };
}
