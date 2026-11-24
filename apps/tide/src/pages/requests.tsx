// SPDX-License-Identifier: Apache-2.0
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { BadgeVariant } from "delino-ui/badge";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { Select, SelectOption } from "delino-ui/select";
import { RequestState, WorkloadKind } from "@delino/tide-protos/v1/common_pb";
import { RunnerProfileService } from "@delino/tide-protos/v1/runner_profile_pb";
import { type Request, WorkService } from "@delino/tide-protos/v1/work_pb";
import { useMemo, useState } from "react";
import { invalidateServices, MAX_PAGE_SIZE, PAGE_SIZE } from "../api.ts";
import { SetupEmpty } from "../components/setup.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  ListPagination,
  PageHeader,
  QueryBoundary,
  ShortId,
  StateBadge,
} from "../components/ui.tsx";
import {
  formatSpan,
  formatTime,
  labels,
  queueWaitMs,
  TERMINAL_REQUEST_STATES,
} from "../format.ts";
import { can, Permission } from "../roles.ts";

const ALL = "all";

const KINDS = [
  WorkloadKind.GITHUB_JOB,
  WorkloadKind.AGENT_SESSION,
  WorkloadKind.DEV_SESSION,
];

const STATES = [
  RequestState.QUEUED,
  RequestState.STARTING,
  RequestState.RUNNING,
  RequestState.ENDED,
  RequestState.CANCELED,
  RequestState.EXPIRED,
  RequestState.FAILED,
];

export function requestStateTone(state: RequestState): BadgeVariant {
  if (state === RequestState.FAILED || state === RequestState.EXPIRED)
    return BadgeVariant.Danger;
  if (state === RequestState.RUNNING) return BadgeVariant.Accent;
  return BadgeVariant.Neutral;
}

/** The source of a request, for display. */
export function requestSource(r: Request): string {
  if (r.githubJob !== undefined) {
    return `${r.githubJob.repositoryFullName} · ${r.githubJob.jobName}`;
  }
  return r.sourceKey;
}

/** Route `requests`: requests with filters; cancel for `admin`. */
export function RequestsPage() {
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const [kind, setKind] = useState<string>(ALL);
  const [state, setState] = useState<string>(ALL);
  const [profileId, setProfileId] = useState<string>(ALL);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [canceling, setCanceling] = useState<Request>();
  const profiles = useQuery(RunnerProfileService.method.listRunnerProfiles, {
    organizationId,
    pageSize: MAX_PAGE_SIZE,
  });
  const profileNames = useMemo(
    () =>
      new Map(
        (profiles.data?.profiles ?? []).map((p) => [p.runnerProfileId, p.name]),
      ),
    [profiles.data],
  );
  const stateFilter = state === ALL ? [] : [Number(state) as RequestState];
  const list = useInfiniteQuery(
    WorkService.method.listRequests,
    {
      organizationId,
      pageSize: PAGE_SIZE,
      pageToken: "",
      kind:
        kind === ALL
          ? WorkloadKind.UNSPECIFIED
          : (Number(kind) as WorkloadKind),
      runnerProfileId: profileId === ALL ? "" : profileId,
      states: stateFilter,
      includeEnded:
        includeEnded || stateFilter.some((s) => TERMINAL_REQUEST_STATES.has(s)),
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const cancel = useMutation(WorkService.method.cancelRequest, {
    onSuccess: () => invalidateServices(queryClient, WorkService),
  });
  const admin = can(role, Permission.CancelRequests);
  const now = Date.now();

  return (
    <>
      <PageHeader
        title="Requests"
        description="Each request is one GitHub Actions job, agent session or dev session. Waiting requests come first, in queue order."
      />
      <div className="filters">
        <Select
          label="Kind"
          value={kind}
          onValueChange={(v) => setKind(v ?? ALL)}
        >
          <SelectOption value={ALL}>All kinds</SelectOption>
          {KINDS.map((k) => (
            <SelectOption key={k} value={String(k)}>
              {labels.workloadKind[k]}
            </SelectOption>
          ))}
        </Select>
        <Select
          label="State"
          value={state}
          onValueChange={(v) => setState(v ?? ALL)}
        >
          <SelectOption value={ALL}>All states</SelectOption>
          {STATES.map((s) => (
            <SelectOption key={s} value={String(s)}>
              {labels.requestState[s]}
            </SelectOption>
          ))}
        </Select>
        <Select
          label="Runner profile"
          value={profileId}
          onValueChange={(v) => setProfileId(v ?? ALL)}
        >
          <SelectOption value={ALL}>All profiles</SelectOption>
          {(profiles.data?.profiles ?? []).map((p) => (
            <SelectOption key={p.runnerProfileId} value={p.runnerProfileId}>
              {p.name}
            </SelectOption>
          ))}
        </Select>
        <Checkbox
          label="Show ended requests"
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      <QueryBoundary query={list} loading="Loading requests">
        {(data) => {
          const requests = data.pages.flatMap((p) => p.requests);
          return requests.length === 0 ? (
            <SetupEmpty
              organizationId={organizationId}
              title="No requests match"
            >
              Requests appear when GitHub queues a job with a tide label, a
              vendor asks for an agent session or a member creates a dev
              session.
            </SetupEmpty>
          ) : (
            <div className="table-wrap">
              <table className="table" aria-label="Requests">
                <thead>
                  <tr>
                    <th scope="col">Request</th>
                    <th scope="col">Kind</th>
                    <th scope="col">Profile</th>
                    <th scope="col">Source</th>
                    <th scope="col">State</th>
                    <th scope="col">End reason</th>
                    <th scope="col">Priority</th>
                    <th scope="col">Queue wait</th>
                    <th scope="col">Queued</th>
                    {admin ? <th scope="col">Actions</th> : null}
                  </tr>
                </thead>
                <tbody>
                  {requests.map((r) => {
                    const wait = queueWaitMs(r, now);
                    return (
                      <tr key={r.requestId}>
                        <td>
                          <ShortId id={r.requestId} />
                        </td>
                        <td>{labels.workloadKind[r.kind]}</td>
                        <td>
                          {profileNames.get(r.runnerProfileId) ?? (
                            <ShortId id={r.runnerProfileId} />
                          )}
                        </td>
                        <td className="wrap">{requestSource(r) || "—"}</td>
                        <td>
                          <StateBadge
                            text={labels.requestState[r.state]}
                            tone={requestStateTone(r.state)}
                          />
                        </td>
                        <td>
                          {TERMINAL_REQUEST_STATES.has(r.state)
                            ? labels.requestEndReason[r.endReason]
                            : "—"}
                        </td>
                        <td>{labels.priority[r.priority]}</td>
                        <td>{wait === undefined ? "—" : formatSpan(wait)}</td>
                        <td>{formatTime(r.queuedAt)}</td>
                        {admin ? (
                          <td>
                            {r.state === RequestState.QUEUED ? (
                              <Button
                                variant={ButtonVariant.Danger}
                                size={ButtonSize.Small}
                                onClick={() => setCanceling(r)}
                              >
                                Cancel
                              </Button>
                            ) : null}
                          </td>
                        ) : null}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
      <ConfirmDialog
        open={canceling !== undefined}
        onOpenChange={(open) => !open && setCanceling(undefined)}
        title="Cancel this request?"
        description="The request ends with Canceled and never gets a runner. A GitHub job stays queued in GitHub until GitHub cancels it."
        confirmLabel="Cancel request"
        onConfirm={() =>
          cancel.mutateAsync({
            organizationId,
            requestId: canceling!.requestId,
          })
        }
      />
    </>
  );
}
