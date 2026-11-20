// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import {
  AgentUpdateState,
  Arch,
  DeviceState,
  HostOs,
} from "@delino/ownsand-sdk/gen/common_pb";
import { DeviceService } from "@delino/ownsand-sdk/gen/organization_pb";
import { Link } from "../router.tsx";
import { PAGE_SIZE, pageOptions } from "../api/paging.ts";
import {
  EmptyState,
  ErrorState,
  LoadMore,
  LoadingState,
  PageHeader,
} from "../components/common.tsx";
import { formatEnum, formatResources } from "../format.ts";
import { RouteId, organizationPath } from "../routes.ts";
import type { OrganizationPageProps } from "./organization-layout.tsx";

export function DevicesPage({ organizationId }: OrganizationPageProps) {
  const query = useInfiniteQuery(
    DeviceService.method.listDevices,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const devices = query.data?.pages.flatMap((page) => page.devices) ?? [];
  return (
    <main className="page">
      <PageHeader
        title="Devices"
        description="Devices enrolled in this organization. Open a device to see its details."
      />
      {query.isPending ? (
        <LoadingState />
      ) : query.isError ? (
        <ErrorState error={query.error} />
      ) : devices.length === 0 ? (
        <EmptyState>
          No devices yet. An admin enrolls a device with an enrollment token.
        </EmptyState>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">State</th>
                <th scope="col">Platform</th>
                <th scope="col">Capacity</th>
                <th scope="col">Limits</th>
                <th scope="col">Allocated</th>
                <th scope="col">Agent</th>
              </tr>
            </thead>
            <tbody>
              {devices.map((device) => (
                <tr key={device.deviceId}>
                  <td>
                    <Link
                      className="text-link"
                      to={organizationPath(
                        organizationId,
                        RouteId.Device,
                        device.deviceId,
                      )}
                    >
                      {device.name || device.deviceId}
                    </Link>
                  </td>
                  <td>{formatEnum(DeviceState, device.state)}</td>
                  <td>
                    {formatEnum(HostOs, device.capabilities?.hostOs ?? 0)} ·{" "}
                    {formatEnum(Arch, device.capabilities?.arch ?? 0)}
                  </td>
                  <td>
                    {device.capabilities
                      ? `${device.capabilities.logicalCpuCount} CPU · ${device.capabilities.memoryMib} MiB`
                      : "—"}
                  </td>
                  <td>{formatResources(device.limits)}</td>
                  <td>{formatResources(device.allocated)}</td>
                  <td>
                    {device.agentVersion || "—"} (
                    {formatEnum(AgentUpdateState, device.updateState)})
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
