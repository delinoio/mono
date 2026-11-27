// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { Role } from "@delino/sandrun-protos/v1/common_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { useEffect, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { TopBar } from "../components/shell.tsx";
import {
  ErrorNotice,
  Loading,
  PageHeader,
  QueryBoundary,
  showsFullError,
} from "../components/ui.tsx";
import type { GitHubCallback } from "../github-callback.ts";
import { useOrganizations } from "../organizations.ts";
import { can, Permission } from "../roles.ts";
import { Link, orgPath, parseRoute, RouteId, useRouter } from "../router.tsx";

/**
 * Links the captured GitHub App installation and continues to integrations.
 * The page links to the sole eligible organization or asks which one to use.
 */
export function GitHubSetupPage({
  callback,
}: {
  callback: GitHubCallback | undefined;
}) {
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const link = useMutation(GitHubService.method.linkGitHubInstallation, {
    onSuccess: () => invalidateServices(queryClient, GitHubService),
  });
  const orgs = useOrganizations(callback !== undefined && link.isIdle);
  const adminOrgs =
    callback !== undefined && orgs.data !== undefined && !showsFullError(orgs)
      ? orgs.data.filter((o) =>
          can(o.callerRole, Permission.ManageIntegrations),
        )
      : [];
  const [selected, setSelected] = useState<string>();
  const eligibleSelection = adminOrgs.find(
    (org) => org.organizationId === selected,
  )?.organizationId;
  // Only a successful full list can show that a previous choice is obsolete.
  if (
    orgs.isSuccess &&
    selected !== undefined &&
    eligibleSelection === undefined
  ) {
    setSelected(undefined);
  }
  // React strict mode runs effects twice; the code and state are single-use.
  const sent = useRef(false);

  const linkTo = (organizationId: string) => {
    if (callback === undefined || sent.current) return;
    sent.current = true;
    link.mutate(
      {
        organizationId,
        githubInstallationId: callback.githubInstallationId,
        code: callback.code,
        state: callback.state,
      },
      {
        // This callback belongs to the visit that submitted the request.
        // Check the address in case the router has yet to render a navigation.
        onSuccess: () => {
          if (parseRoute(window.location.pathname).id === RouteId.GitHubSetup) {
            navigate(orgPath(RouteId.Integrations, organizationId), {
              replace: true,
            });
          }
        },
      },
    );
  };

  const onlyOrg = adminOrgs.length === 1 ? adminOrgs[0] : undefined;
  useEffect(() => {
    if (onlyOrg !== undefined) linkTo(onlyOrg.organizationId);
    // Runs once per organization list; linkTo sends the single-use code at
    // most once.
  }, [onlyOrg?.organizationId]);

  let body;
  if (callback === undefined) {
    body = (
      <div className="notice notice-error" role="alert">
        <p>
          The address has no GitHub installation details. Start the installation
          again from the integrations of your organization.
        </p>
        <Link to="/">Go to your organizations</Link>
      </div>
    );
  } else if (link.error) {
    body = (
      <>
        <ErrorNotice error={link.error} />
        <p className="muted">
          The installation link is single-use. Start the installation again from
          the integrations of your organization.
        </p>
        <Link to="/">Go to your organizations</Link>
      </>
    );
  } else if (link.isPending || sent.current) {
    body = <Loading label="Linking the GitHub installation" />;
  } else {
    body = (
      <QueryBoundary query={orgs} loading="Loading your organizations">
        {() => {
          if (adminOrgs.length === 0) {
            return (
              <div className="notice notice-error" role="alert">
                <p>
                  Only an owner or admin of a SandRun organization can link a
                  GitHub installation.
                </p>
                <Link to="/">Go to your organizations</Link>
              </div>
            );
          }
          if (onlyOrg !== undefined) {
            return <Loading label="Linking the GitHub installation" />;
          }
          return (
            <form
              className="form"
              onSubmit={(event) => {
                event.preventDefault();
                if (eligibleSelection !== undefined) linkTo(eligibleSelection);
              }}
            >
              <Select
                label="SandRun organization"
                value={eligibleSelection ?? null}
                onValueChange={(value) => setSelected(value ?? undefined)}
              >
                {adminOrgs.map((o) => (
                  <SelectOption key={o.organizationId} value={o.organizationId}>
                    {`${o.name} (${o.callerRole === Role.OWNER ? "owner" : "admin"})`}
                  </SelectOption>
                ))}
              </Select>
              <div className="actions">
                <Button
                  type="submit"
                  disabled={eligibleSelection === undefined}
                >
                  Link installation
                </Button>
              </div>
            </form>
          );
        }}
      </QueryBoundary>
    );
  }

  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <PageHeader
          title="Connect GitHub"
          description="Link the GitHub organization where you installed the SandRun GitHub App."
        />
        {body}
      </main>
    </>
  );
}
