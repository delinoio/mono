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
import { RunnerState } from "@delino/tide-protos/v1/common_pb";
import { RunnerProfileService } from "@delino/tide-protos/v1/runner_profile_pb";
import { type Runner, WorkService } from "@delino/tide-protos/v1/work_pb";
import { useMemo, useState } from "react";
import { invalidateServices, MAX_PAGE_SIZE, PAGE_SIZE } from "../api.ts";
import { SetupEmpty } from "../components/setup.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  ErrorNotice,
  Loading,
  PageHeader,
  ShortId,
  StateBadge,
} from "../components/ui.tsx";
import { formatTime, labels, TERMINAL_RUNNER_STATES } from "../format.ts";
import { can, Permission } from "../roles.ts";

const ALL = "all";

const STATES = [
  RunnerState.CREATING,
  RunnerState.PREPARING,
  RunnerState.WARM,
  RunnerState.WAITING_FOR_WORK,
  RunnerState.BUSY,
  RunnerState.DELETING,
  RunnerState.DELETED,
  RunnerState.FAILED,
];

function runnerTone(state: RunnerState): BadgeVariant {
  if (state === RunnerState.FAILED) return BadgeVariant.Danger;
  if (state === RunnerState.BUSY) return BadgeVariant.Accent;
  return BadgeVariant.Neutral;
}

/** Route `runners`: runners with profile, device, state and end reason. */
export function RunnersPage() {
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const [state, setState] = useState<string>(ALL);
  const [profileId, setProfileId] = useState<string>(ALL);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [deleting, setDeleting] = useState<Runner>();
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
  const devices = useQuery(WorkService.method.getCapacity, { organizationId });
  const deviceNames = useMemo(
    () =>
      new Map(
        (devices.data?.devices ?? []).map((d) => [d.ownsandDeviceId, d.name]),
      ),
    [devices.data],
  );
  const stateFilter = state === ALL ? [] : [Number(state) as RunnerState];
  const list = useInfiniteQuery(
    WorkService.method.listRunners,
    {
      organizationId,
      pageSize: PAGE_SIZE,
      pageToken: "",
      runnerProfileId: profileId === ALL ? "" : profileId,
      states: stateFilter,
      includeEnded:
        includeEnded || stateFilter.some((s) => TERMINAL_RUNNER_STATES.has(s)),
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const remove = useMutation(WorkService.method.deleteRunner, {
    onSuccess: () => invalidateServices(queryClient, WorkService),
  });
  const runners = list.data?.pages.flatMap((p) => p.runners) ?? [];
  const admin = can(role, Permission.DeleteRunners);

  return (
    <>
      <PageHeader
        title="Runners"
        description="Each runner is one OwnSand sandbox that serves at most one request. tide deletes it when the work ends."
      />
      <div className="filters">
        <Select
          label="State"
          value={state}
          onValueChange={(v) => setState(v ?? ALL)}
        >
          <SelectOption value={ALL}>All states</SelectOption>
          {STATES.map((s) => (
            <SelectOption key={s} value={String(s)}>
              {labels.runnerState[s]}
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
          label="Show ended runners"
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      {list.isPending ? (
        <Loading label="Loading runners" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={() => void list.refetch()} />
      ) : runners.length === 0 ? (
        <SetupEmpty organizationId={organizationId} title="No runners match">
          Runners start for waiting requests and for the warm count of each
          runner profile.
        </SetupEmpty>
      ) : (
        <div className="table-wrap">
          <table className="table" aria-label="Runners">
            <thead>
              <tr>
                <th scope="col">Runner</th>
                <th scope="col">Profile</th>
                <th scope="col">Kind</th>
                <th scope="col">Device</th>
                <th scope="col">State</th>
                <th scope="col">End reason</th>
                <th scope="col">Created</th>
                {admin ? <th scope="col">Actions</th> : null}
              </tr>
            </thead>
            <tbody>
              {runners.map((r) => {
                const ended = TERMINAL_RUNNER_STATES.has(r.state);
                return (
                  <tr key={r.runnerId}>
                    <td>
                      <ShortId id={r.runnerId} />
                    </td>
                    <td>
                      {profileNames.get(r.runnerProfileId) ?? (
                        <ShortId id={r.runnerProfileId} />
                      )}
                    </td>
                    <td>
                      {labels.workloadKind[r.kind]} ·{" "}
                      {labels.guestOs[r.guestOs]}
                    </td>
                    <td>
                      {r.ownsandDeviceId === ""
                        ? "—"
                        : (deviceNames.get(r.ownsandDeviceId) ?? (
                            <ShortId id={r.ownsandDeviceId} />
                          ))}
                    </td>
                    <td>
                      <StateBadge
                        text={labels.runnerState[r.state]}
                        tone={runnerTone(r.state)}
                      />
                    </td>
                    <td className="wrap">
                      {ended
                        ? `${labels.runnerEndReason[r.endReason]}${r.ownsandEndReason ? ` (${r.ownsandEndReason})` : ""}`
                        : "—"}
                    </td>
                    <td>{formatTime(r.createdAt)}</td>
                    {admin ? (
                      <td>
                        {!ended && r.state !== RunnerState.DELETING ? (
                          <Button
                            variant={ButtonVariant.Danger}
                            size={ButtonSize.Small}
                            onClick={() => setDeleting(r)}
                          >
                            Delete
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
      )}
      {list.hasNextPage ? (
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => void list.fetchNextPage()}
        >
          Show more
        </Button>
      ) : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title="Delete this runner?"
        description="tide deletes the runner and its OwnSand sandbox. Work that runs in it stops at once. This cannot be undone."
        confirmLabel="Delete runner"
        onConfirm={() =>
          remove.mutateAsync({ organizationId, runnerId: deleting!.runnerId })
        }
      />
    </>
  );
}
