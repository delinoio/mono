// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery, useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { Select, SelectOption } from "delino-ui/select";
import {
  type DevSession,
  DevSessionService,
} from "@delino/tide-protos/v1/dev_session_pb";
import { WorkService } from "@delino/tide-protos/v1/work_pb";
import { useMemo, useState } from "react";
import { invalidateServices, PAGE_SIZE } from "../api.ts";
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
  formatTime,
  labels,
  memberName,
  TERMINAL_REQUEST_STATES,
} from "../format.ts";
import { useMembers } from "../members.ts";
import { canDeleteDevSession } from "../roles.ts";
import { Link, orgPath, RouteId } from "../router.tsx";
import { requestStateTone } from "./requests.tsx";

/** Owner filter of the dev session list. */
export enum OwnerFilter {
  Everyone = "everyone",
  Mine = "mine",
}

/**
 * Route `dev-sessions`: dev session metadata of the organization. It shows
 * no command, output or file content and runs no command; commands and
 * file transfers are available only through the CLI, the MCP server and
 * the SDKs.
 */
export function DevSessionsPage() {
  const { organizationId, role, userId } = useOrg();
  const queryClient = useQueryClient();
  const [owner, setOwner] = useState<OwnerFilter>(OwnerFilter.Everyone);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [deleting, setDeleting] = useState<DevSession>();
  const { members } = useMembers(organizationId);
  const names = useMemo(
    () => new Map(members.map((member) => [member.userId, member.displayName])),
    [members],
  );
  const list = useInfiniteQuery(
    DevSessionService.method.listDevSessions,
    {
      organizationId,
      pageSize: PAGE_SIZE,
      pageToken: "",
      owner:
        owner === OwnerFilter.Mine
          ? { case: "ownedByCaller", value: true }
          : { case: undefined },
      includeEnded,
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const remove = useMutation(DevSessionService.method.deleteDevSession, {
    onSuccess: () =>
      invalidateServices(queryClient, DevSessionService, WorkService),
  });

  return (
    <>
      <PageHeader
        title="Dev sessions"
        description="A dev session is a sandbox of one user for builds and tests on another guest OS. Run commands and copy files with the tide CLI, the tide MCP server or the SDKs; the console shows no commands, output or files."
      />
      <div className="filters">
        <Select
          label="Owner"
          value={owner}
          onValueChange={(v) =>
            setOwner((v as OwnerFilter) ?? OwnerFilter.Everyone)
          }
        >
          <SelectOption value={OwnerFilter.Everyone}>Everyone</SelectOption>
          <SelectOption value={OwnerFilter.Mine}>Only mine</SelectOption>
        </Select>
        <Checkbox
          label="Show ended dev sessions"
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      <QueryBoundary query={list} loading="Loading dev sessions">
        {(data) => {
          const sessions = data.pages.flatMap((p) => p.devSessions);
          return sessions.length === 0 ? (
            <SetupEmpty organizationId={organizationId} title="No dev sessions">
              Create a dev session with <code>tide session create</code> or the
              tide MCP server, with a dev session runner profile.{" "}
              <Link to={orgPath(RouteId.ApiKeys, organizationId)}>
                Create a personal API key
              </Link>{" "}
              for a coding agent.
            </SetupEmpty>
          ) : (
            <div className="table-wrap">
              <table className="table" aria-label="Dev sessions">
                <thead>
                  <tr>
                    <th scope="col">Dev session</th>
                    <th scope="col">Owner</th>
                    <th scope="col">Profile</th>
                    <th scope="col">Guest OS</th>
                    <th scope="col">State</th>
                    <th scope="col">End reason</th>
                    <th scope="col">Queued</th>
                    <th scope="col">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {sessions.map((s) => {
                    const ended = TERMINAL_REQUEST_STATES.has(s.state);
                    return (
                      <tr key={s.devSessionId}>
                        <td>
                          {s.label !== "" ? `${s.label} ` : null}
                          <ShortId id={s.devSessionId} />
                        </td>
                        <td>
                          {memberName(names, s.ownerUserId)}
                          {s.ownerUserId === userId ? (
                            <span className="muted"> (you)</span>
                          ) : null}
                        </td>
                        <td>
                          {s.runnerProfileName || (
                            <ShortId id={s.runnerProfileId} />
                          )}
                        </td>
                        <td>{labels.guestOs[s.guestOs]}</td>
                        <td>
                          <StateBadge
                            text={labels.requestState[s.state]}
                            tone={requestStateTone(s.state)}
                          />
                        </td>
                        <td>
                          {ended ? labels.requestEndReason[s.endReason] : "—"}
                        </td>
                        <td>{formatTime(s.queuedAt)}</td>
                        <td>
                          {!ended && canDeleteDevSession(role, userId, s) ? (
                            <Button
                              variant={ButtonVariant.Danger}
                              size={ButtonSize.Small}
                              onClick={() => setDeleting(s)}
                            >
                              Delete
                            </Button>
                          ) : null}
                        </td>
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
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title="Delete this dev session?"
        description="A queued dev session is canceled. A starting or running one ends after tide deletes its sandbox; running commands and files in it are lost. This cannot be undone."
        confirmLabel="Delete dev session"
        onConfirm={() =>
          remove.mutateAsync({
            organizationId,
            devSessionId: deleting!.devSessionId,
          })
        }
      />
    </>
  );
}
