// SPDX-License-Identifier: Apache-2.0
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { BadgeVariant } from "delino-ui/badge";
import {
  DeviceState,
  OwnSandLinkState,
  RunnerState,
} from "@delino/sandrun-protos/v1/common_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import {
  type DeviceCapacity,
  WorkService,
} from "@delino/sandrun-protos/v1/work_pb";
import { useEffect, useMemo, useReducer, useState } from "react";
import { watchQueue } from "sandrun-sdk";
import { SetupGuide } from "../components/setup.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  Loading,
  PageHeader,
  QueryBoundary,
  ShortId,
  StateBadge,
} from "../components/ui.tsx";
import { ownsandDevicesUrl } from "../config.ts";
import { formatSpan, labels, queueWaitMs } from "../format.ts";
import {
  applyQueueEvent,
  initialQueueView,
  LiveState,
  QueueViewActionKind,
  waitingInQueueOrder,
} from "../queue.ts";
import { useRunnerProfiles } from "../runner-profiles.ts";
import { Link, orgPath, RouteId } from "../router.tsx";

const RUNNER_ORDER = [
  RunnerState.CREATING,
  RunnerState.PREPARING,
  RunnerState.WARM,
  RunnerState.WAITING_FOR_WORK,
  RunnerState.BUSY,
  RunnerState.DELETING,
];

const LIVE_TEXT: Record<LiveState, string> = {
  [LiveState.Connecting]: "Connecting",
  [LiveState.Live]: "Live",
  [LiveState.Reconnecting]: "Reconnecting",
  [LiveState.Disconnected]: "Disconnected",
};

/** How often the waiting time of queued requests is redrawn. */
const CLOCK_TICK_MS = 15_000;

/**
 * Route `overview`: the live queue from `WatchQueue` (reconnects with the
 * SDK helper), runners by state and the device pool from `GetCapacity`.
 */
export function OverviewPage() {
  const { organizationId } = useOrg();
  const transport = useTransport();
  const [view, dispatch] = useReducer(
    applyQueueEvent,
    undefined,
    initialQueueView,
  );
  const [streamError, setStreamError] = useState<unknown>();
  const [attempt, setAttempt] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), CLOCK_TICK_MS);
    return () => clearInterval(timer);
  }, []);
  useEffect(() => {
    const abort = new AbortController();
    const work = createClient(WorkService, transport);
    // A new iterator sends no initial gap, so start a new snapshot here.
    // Otherwise a retry merges its snapshot into the old state and keeps
    // requests and runners that ended while no stream was open.
    dispatch({ kind: QueueViewActionKind.Restart });
    void (async () => {
      try {
        for await (const event of watchQueue(work, {
          organizationId,
          signal: abort.signal,
        })) {
          dispatch(event);
        }
      } catch (error) {
        if (!abort.signal.aborted) {
          dispatch({ kind: QueueViewActionKind.StreamFailed });
          setStreamError(error);
        }
      }
    })();
    return () => abort.abort();
  }, [transport, organizationId, attempt]);

  const profiles = useRunnerProfiles(organizationId);
  const profileNames = useMemo(
    () =>
      new Map((profiles.data ?? []).map((p) => [p.runnerProfileId, p.name])),
    [profiles.data],
  );
  const link = useQuery(OwnSandLinkService.method.getOwnSandLink, {
    organizationId,
  });

  const waiting = waitingInQueueOrder(view.requests.values());
  const running = [...view.requests.values()].filter(
    (r) => !waiting.includes(r),
  );
  const runnersByState = RUNNER_ORDER.map((state) => ({
    state,
    runners: [...view.runners.values()].filter((r) => r.state === state),
  }));

  return (
    <>
      <PageHeader
        title="Overview"
        description="The queue of every workload kind, the runners and the device pool of the linked OwnSand organization."
        action={
          <span className="live" role="status" aria-label="Queue updates">
            <StateBadge
              text={LIVE_TEXT[view.live]}
              tone={
                view.live === LiveState.Live
                  ? BadgeVariant.Accent
                  : BadgeVariant.Neutral
              }
            />
          </span>
        }
      />
      <SetupGuide organizationId={organizationId} />
      {streamError !== undefined ? (
        <ErrorNotice
          error={streamError}
          onRetry={() => {
            setStreamError(undefined);
            setAttempt((n) => n + 1);
          }}
        />
      ) : null}
      <section className="section" aria-labelledby="queue-title">
        <h2 id="queue-title">Waiting requests</h2>
        {profiles.error ? (
          <ErrorNotice
            title="Could not load runner profiles."
            error={profiles.error}
            onRetry={() => void profiles.refetch()}
          />
        ) : null}
        {view.live === LiveState.Connecting ? (
          <Loading label="Loading the queue" />
        ) : waiting.length === 0 ? (
          <Empty title="No request is waiting">
            New GitHub jobs, agent sessions and dev sessions appear here in
            queue order.
          </Empty>
        ) : (
          <div
            className="table-wrap"
            role="region"
            aria-label="Waiting requests table"
            tabIndex={0}
          >
            <table className="table" aria-label="Waiting requests">
              <thead>
                <tr>
                  <th scope="col">Position</th>
                  <th scope="col">Request</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Profile</th>
                  <th scope="col">Priority</th>
                  <th scope="col">Waiting</th>
                </tr>
              </thead>
              <tbody>
                {waiting.map((r, index) => (
                  <tr key={r.requestId}>
                    <td>{index + 1}</td>
                    <td>
                      <ShortId id={r.requestId} />
                    </td>
                    <td>{labels.workloadKind[r.kind]}</td>
                    <td>
                      {profileNames.get(r.runnerProfileId) ?? (
                        <ShortId id={r.runnerProfileId} />
                      )}
                    </td>
                    <td>{labels.priority[r.priority]}</td>
                    <td>{formatSpan(queueWaitMs(r, now) ?? 0)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {running.length > 0 ? (
          <p className="muted">
            {running.length}{" "}
            {running.length === 1 ? "request is" : "requests are"} starting or
            running.
          </p>
        ) : null}
      </section>
      <section className="section" aria-labelledby="runners-title">
        <h2 id="runners-title">Runners by state</h2>
        {view.live === LiveState.Connecting ? (
          <Loading label="Loading the runners" />
        ) : (
          <dl className="details details-flat">
            {runnersByState.map(({ state, runners }) => (
              <div className="detail" key={state}>
                <dt>{labels.runnerState[state]}</dt>
                <dd>{runners.length}</dd>
              </div>
            ))}
          </dl>
        )}
      </section>
      <section className="section" aria-labelledby="devices-title">
        <h2 id="devices-title">Device pool</h2>
        <p className="muted">
          Devices, their limits and the vCPU overcommit are managed in the
          OwnSand console.
          {link.data?.link !== undefined ? (
            <>
              {" "}
              <a
                href={ownsandDevicesUrl(link.data.link.ownsandOrganizationId)}
                target="_blank"
                rel="noreferrer"
              >
                Manage devices in OwnSand
              </a>
            </>
          ) : null}
        </p>
        <QueryBoundary
          query={link}
          loading="Loading the device pool"
          errorTitle="Could not load the OwnSand link."
        >
          {({ link: ownSandLink }) =>
            ownSandLink?.state === OwnSandLinkState.ACTIVE ? (
              <DeviceCapacityTable organizationId={organizationId} />
            ) : (
              <Empty title="OwnSand link required">
                {ownSandLink === undefined
                  ? "An admin must link an OwnSand organization to show its devices."
                  : "An admin must replace the OwnSand API key to show its devices."}{" "}
                <Link to={orgPath(RouteId.Integrations, organizationId)}>
                  Go to integrations
                </Link>
              </Empty>
            )
          }
        </QueryBoundary>
      </section>
    </>
  );
}

function DeviceCapacityTable({ organizationId }: { organizationId: string }) {
  const capacity = useQuery(WorkService.method.getCapacity, { organizationId });
  return (
    <QueryBoundary query={capacity} loading="Loading the device pool">
      {(data) =>
        data.devices.length === 0 ? (
          <Empty title="No devices">
            Link an OwnSand organization with devices, or enroll a device in the
            OwnSand console.
          </Empty>
        ) : (
          <div
            className="table-wrap"
            role="region"
            aria-label="Devices table"
            tabIndex={0}
          >
            <table className="table" aria-label="Devices">
              <thead>
                <tr>
                  <th scope="col">Device</th>
                  <th scope="col">State</th>
                  <th scope="col">Platform</th>
                  <th scope="col">vCPUs</th>
                  <th scope="col">Memory</th>
                  <th scope="col">Disk</th>
                  <th scope="col">SandRun runners</th>
                </tr>
              </thead>
              <tbody>
                {data.devices.map((d) => (
                  <DeviceRow key={d.ownsandDeviceId} device={d} />
                ))}
              </tbody>
            </table>
          </div>
        )
      }
    </QueryBoundary>
  );
}

function DeviceRow({ device: d }: { device: DeviceCapacity }) {
  const runners = d.runners.reduce((sum, r) => sum + r.count, 0);
  return (
    <tr>
      <td>{d.name || <ShortId id={d.ownsandDeviceId} />}</td>
      <td>
        <StateBadge
          text={labels.deviceState[d.state]}
          tone={
            d.state === DeviceState.ONLINE
              ? BadgeVariant.Accent
              : BadgeVariant.Neutral
          }
        />
      </td>
      <td>
        {d.hostOs} · {d.arch}
      </td>
      <td>
        {String(d.allocation?.vcpuCount ?? 0n)} of{" "}
        {String(d.effectiveVcpuLimit)}
        {d.limits !== undefined && d.limits.vcpuOvercommitPercent !== 100 ? (
          <span className="muted">
            {" "}
            ({d.limits.vcpuCount} × {d.limits.vcpuOvercommitPercent}%)
          </span>
        ) : null}
      </td>
      <td>
        {String(d.allocation?.memoryMib ?? 0n)} of{" "}
        {String(d.limits?.memoryMib ?? 0n)} MiB
      </td>
      <td>
        {String(d.allocation?.diskGib ?? 0n)} of{" "}
        {String(d.limits?.diskGib ?? 0n)} GiB
      </td>
      <td>
        {runners}
        {d.runners.length > 0 ? (
          <span className="muted">
            {" "}
            (
            {d.runners
              .filter((r) => r.count > 0)
              .map((r) => `${labels.runnerState[r.state]} ${r.count}`)
              .join(", ")}
            )
          </span>
        ) : null}
      </td>
    </tr>
  );
}
