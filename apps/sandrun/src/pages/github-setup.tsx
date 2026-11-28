// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { Role } from "@delino/sandrun-protos/v1/common_pb";
import {
  GitHubInstallOrganizationMismatchSchema,
  GitHubService,
} from "@delino/sandrun-protos/v1/github_pb";
import { useCallback, useEffect, useRef, useState } from "react";
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
import { useI18n } from "../i18n.ts";
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
  const { t } = useI18n();
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
  const [organizationMismatches, setOrganizationMismatches] = useState(0);
  const requiresOrganizationChoice = organizationMismatches > 0;
  const focusPicker = useCallback((form: HTMLFormElement | null) => {
    if (form === null) return;
    const frame = requestAnimationFrame(() => {
      form.querySelector<HTMLElement>('[role="combobox"]')?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, []);
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
        onError: (error) => {
          const failure = ConnectError.from(error);
          if (
            failure.code === Code.InvalidArgument &&
            failure.findDetails(GitHubInstallOrganizationMismatchSchema)
              .length > 0
          ) {
            setSelected(undefined);
            // Remount and focus the cleared picker even if the response arrived
            // before React showed the pending state.
            setOrganizationMismatches((count) => count + 1);
            sent.current = false;
            link.reset();
          }
        },
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

  const automaticOrg =
    !requiresOrganizationChoice && adminOrgs.length === 1
      ? adminOrgs[0]
      : undefined;
  useEffect(() => {
    if (automaticOrg !== undefined) linkTo(automaticOrg.organizationId);
    // After a mismatch, the user must submit the picker explicitly.
  }, [automaticOrg?.organizationId]);

  let body;
  if (callback === undefined) {
    body = (
      <div className="notice notice-error" role="alert">
        <p>{t("githubSetup.missingDetails")}</p>
        <Link to="/">{t("common.goToOrganizations")}</Link>
      </div>
    );
  } else if (link.error) {
    body = (
      <>
        <ErrorNotice error={link.error} />
        <p className="muted">{t("githubSetup.singleUse")}</p>
        <Link to="/">{t("common.goToOrganizations")}</Link>
      </>
    );
  } else if (link.isPending || sent.current) {
    body = <Loading label={t("githubSetup.linking")} />;
  } else {
    body = (
      <QueryBoundary
        query={orgs}
        loading={t("githubSetup.loadingOrganizations")}
      >
        {() => {
          if (adminOrgs.length === 0) {
            return (
              <div className="notice notice-error" role="alert">
                <p>{t("githubSetup.notAdmin")}</p>
                <Link to="/">{t("common.goToOrganizations")}</Link>
              </div>
            );
          }
          if (automaticOrg !== undefined) {
            return <Loading label={t("githubSetup.linking")} />;
          }
          return (
            <form
              key={organizationMismatches}
              className="form"
              ref={focusPicker}
              onSubmit={(event) => {
                event.preventDefault();
                if (eligibleSelection !== undefined) linkTo(eligibleSelection);
              }}
            >
              {requiresOrganizationChoice ? (
                <div className="notice notice-error" role="alert">
                  {t("githubSetup.organizationMismatch")}
                </div>
              ) : null}
              <Select
                label={t("githubSetup.organization")}
                value={eligibleSelection ?? null}
                onValueChange={(value) => setSelected(value ?? undefined)}
              >
                {adminOrgs.map((o) => (
                  <SelectOption key={o.organizationId} value={o.organizationId}>
                    {t(
                      o.callerRole === Role.OWNER
                        ? "githubSetup.organizationOwner"
                        : "githubSetup.organizationAdmin",
                      { name: o.name },
                    )}
                  </SelectOption>
                ))}
              </Select>
              <div className="actions">
                <Button
                  type="submit"
                  disabled={eligibleSelection === undefined}
                >
                  {t("githubSetup.link")}
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
          title={t("githubSetup.title")}
          description={t("githubSetup.description")}
        />
        {body}
      </main>
    </>
  );
}
