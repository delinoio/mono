// SPDX-License-Identifier: Apache-2.0
import {
  callUnaryMethod,
  createConnectQueryKey,
  useTransport,
} from "@connectrpc/connect-query";
import {
  type RunnerProfile,
  RunnerProfileService,
} from "@delino/sandrun-protos/v1/runner_profile_pb";
import { type QueryClient, useQuery } from "@tanstack/react-query";
import { invalidateServices, MAX_PAGE_SIZE } from "./api.ts";

const listRunnerProfiles = RunnerProfileService.method.listRunnerProfiles;

/** Shares the complete runner profile list for an organization. */
export function useRunnerProfiles(organizationId: string) {
  const transport = useTransport();
  return useQuery({
    queryKey: [
      ...createConnectQueryKey({
        schema: listRunnerProfiles,
        input: { organizationId },
        cardinality: "finite",
        transport,
      }),
      "all-pages",
    ],
    queryFn: async ({ signal }) => {
      const profiles: RunnerProfile[] = [];
      let pageToken = "";
      do {
        const page = await callUnaryMethod(
          transport,
          listRunnerProfiles,
          { organizationId, pageSize: MAX_PAGE_SIZE, pageToken },
          { signal },
        );
        profiles.push(...page.profiles);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return profiles;
    },
  });
}

/** Restarts lists that may have started before a profile mutation completed. */
export async function refreshRunnerProfiles(queryClient: QueryClient) {
  await queryClient.cancelQueries({
    queryKey: createConnectQueryKey({
      schema: listRunnerProfiles,
      cardinality: undefined,
    }),
  });
  await invalidateServices(queryClient, RunnerProfileService);
}
