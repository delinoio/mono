// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { GuestOs, SandboxState } from "@delino/ownsand-sdk/gen/common_pb";
import { SandboxService } from "@delino/ownsand-sdk/gen/sandbox_pb";
import { PAGE_SIZE, pageOptions } from "../api/paging.ts";
import {
  EmptyState,
  ErrorState,
  LoadMore,
  LoadingState,
  PageHeader,
} from "../components/common.tsx";
import { formatEnum, formatResources, formatTime } from "../format.ts";
import { Link } from "../router.tsx";
import { RouteId, organizationPath } from "../routes.ts";
import type { OrganizationPageProps } from "./organization-layout.tsx";

export function SandboxesPage({ organizationId }: OrganizationPageProps) {
  const query = useInfiniteQuery(
    SandboxService.method.listSandboxes,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const sandboxes = query.data?.pages.flatMap((page) => page.sandboxes) ?? [];
  return (
    <main className="page">
      <PageHeader
        title="Sandboxes"
        description="Sandboxes run on your organization's devices. Create and delete them with the SDK or the CLI."
      />
      {query.isPending ? (
        <LoadingState />
      ) : query.isError ? (
        <ErrorState error={query.error} />
      ) : sandboxes.length === 0 ? (
        <EmptyState>No sandboxes yet.</EmptyState>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Sandbox</th>
                <th scope="col">State</th>
                <th scope="col">Device</th>
                <th scope="col">Guest</th>
                <th scope="col">Resources</th>
                <th scope="col">Idle deadline</th>
                <th scope="col">Max deadline</th>
                <th scope="col">Labels</th>
              </tr>
            </thead>
            <tbody>
              {sandboxes.map((sandbox) => (
                <tr key={sandbox.sandboxId}>
                  <td className="mono">{sandbox.sandboxId}</td>
                  <td>{formatEnum(SandboxState, sandbox.state)}</td>
                  <td>
                    <Link
                      className="text-link mono"
                      to={organizationPath(
                        organizationId,
                        RouteId.Device,
                        sandbox.deviceId,
                      )}
                    >
                      {sandbox.deviceId}
                    </Link>
                  </td>
                  <td>{formatEnum(GuestOs, sandbox.guestOs)}</td>
                  <td>{formatResources(sandbox.resources)}</td>
                  <td>{formatTime(sandbox.idleDeadline)}</td>
                  <td>{formatTime(sandbox.maxDeadline)}</td>
                  <td>
                    {Object.entries(sandbox.labels).length === 0 ? (
                      "—"
                    ) : (
                      <ul className="labels">
                        {Object.entries(sandbox.labels).map(([key, value]) => (
                          <li key={key} className="label-chip">
                            {key}={value}
                          </li>
                        ))}
                      </ul>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <LoadMore
        hasMore={query.hasNextPage}
        loading={query.isFetchingNextPage}
        onLoadMore={() => void query.fetchNextPage()}
      />
    </main>
  );
}
