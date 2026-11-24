// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { Role } from "@delino/tide-protos/v1/common_pb";
import { GitHubService } from "@delino/tide-protos/v1/github_pb";
import { useEffect, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { TopBar } from "../components/shell.tsx";
import { ErrorNotice, Loading, PageHeader } from "../components/ui.tsx";
import {
  type GitHubCallback,
  parseGitHubCallback,
  stripCallbackFromAddress,
} from "../github-callback.ts";
import { useOrganizations } from "../organizations.ts";
import { can, Permission } from "../roles.ts";
import { Link, orgPath, parseRoute, RouteId, useRouter } from "../router.tsx";

/**
 * Route `github-setup`: the callback of the GitHub App installation. It
 * reads `installation_id`, `code` and `state`, removes them from the address
 * bar, calls `LinkGitHubInstallation` and continues to `integrations`. The
 * callback URL does not name the tide organization, so the page links to
 * the only organization in which the user is an admin, or asks which one.
 */
export function GitHubSetupPage() {
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const [callback] = useState<GitHubCallback | undefined>(() =>
    parseGitHubCallback(window.location.search),
  );
  useEffect(() => {
    stripCallbackFromAddress(window.location, window.history);
  }, []);
  const orgs = useOrganizations(callback !== undefined);
  const loadingOrgs = orgs.isPending || orgs.fetchStatus !== "idle";
  const adminOrgs =
    orgs.isSuccess && !loadingOrgs
      ? orgs.data.filter((o) =>
          can(o.callerRole, Permission.ManageIntegrations),
        )
      : [];
  const link = useMutation(GitHubService.method.linkGitHubInstallation, {
    onSuccess: () => invalidateServices(queryClient, GitHubService),
  });
  const [selected, setSelected] = useState<string>();
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
  } else if (loadingOrgs) {
    body = <Loading label="Loading your organizations" />;
  } else if (orgs.error) {
    body = (
      <ErrorNotice error={orgs.error} onRetry={() => void orgs.refetch()} />
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
  } else if (adminOrgs.length === 0) {
    body = (
      <div className="notice notice-error" role="alert">
        <p>
          Only an owner or admin of a tide organization can link a GitHub
          installation.
        </p>
        <Link to="/">Go to your organizations</Link>
      </div>
    );
  } else if (onlyOrg !== undefined || link.isPending || sent.current) {
    body = <Loading label="Linking the GitHub installation" />;
  } else {
    body = (
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
          if (selected !== undefined) linkTo(selected);
        }}
      >
        <Select
          label="tide organization"
          value={selected ?? null}
          onValueChange={(v) => v && setSelected(v)}
        >
          {adminOrgs.map((o) => (
            <SelectOption key={o.organizationId} value={o.organizationId}>
              {`${o.name} (${o.callerRole === Role.OWNER ? "owner" : "admin"})`}
            </SelectOption>
          ))}
        </Select>
        <div className="actions">
          <Button type="submit" disabled={selected === undefined}>
            Link installation
          </Button>
        </div>
      </form>
    );
  }

  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <PageHeader
          title="Connect GitHub"
          description="Link the GitHub organization where you installed the tide GitHub App."
        />
        {body}
      </main>
    </>
  );
}
