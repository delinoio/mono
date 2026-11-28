// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { AgentIntegrationService } from "@delino/sandrun-protos/v1/agent_integration_pb";
import { OwnSandLinkState } from "@delino/sandrun-protos/v1/common_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/sandrun-protos/v1/runner_profile_pb";
import { type MessageKey, useI18n } from "../i18n.ts";
import { Link, orgPath, RouteId } from "../router.tsx";
import { Empty } from "./ui.tsx";

/** The steps of the first setup, in the order of the console contract. */
export enum SetupStep {
  LinkOwnSand = "link_ownsand",
  ConnectWorkSource = "connect_work_source",
  CreateRunnerProfile = "create_runner_profile",
  Done = "done",
}

interface SetupState {
  linkState: OwnSandLinkState | null | undefined;
  hasGitHubInstallation: boolean | undefined;
  hasAgentIntegration: boolean | undefined;
  hasRunnerProfile: boolean | undefined;
}

/** Returns the next step when its prerequisites are known. */
export function nextSetupStep(state: SetupState): SetupStep | undefined {
  if (state.linkState === undefined) return undefined;
  if (state.linkState !== OwnSandLinkState.ACTIVE) return SetupStep.LinkOwnSand;
  if (
    state.hasGitHubInstallation === false &&
    state.hasAgentIntegration === false
  ) {
    return SetupStep.ConnectWorkSource;
  }
  if (state.hasGitHubInstallation || state.hasAgentIntegration) {
    if (state.hasRunnerProfile === undefined) return undefined;
    return state.hasRunnerProfile
      ? SetupStep.Done
      : SetupStep.CreateRunnerProfile;
  }
  return undefined;
}

interface StepText {
  title: MessageKey;
  detail: MessageKey;
  /** The sentence that names the step as the next one. */
  next: MessageKey;
  route: RouteId.Integrations | RouteId.Profiles;
  action?: MessageKey;
}

const STEP_TEXT = {
  [SetupStep.LinkOwnSand]: {
    title: "setup.linkOwnSand.title",
    detail: "setup.linkOwnSand.detail",
    next: "setup.linkOwnSand.next",
    route: RouteId.Integrations,
  },
  [SetupStep.ConnectWorkSource]: {
    title: "setup.connectWorkSource.title",
    detail: "setup.connectWorkSource.detail",
    next: "setup.connectWorkSource.next",
    route: RouteId.Integrations,
  },
  [SetupStep.CreateRunnerProfile]: {
    title: "setup.createRunnerProfile.title",
    detail: "setup.createRunnerProfile.detail",
    next: "setup.createRunnerProfile.next",
    route: RouteId.Profiles,
  },
} as const satisfies Record<Exclude<SetupStep, SetupStep.Done>, StepText>;

const INVALID_LINK_TEXT: StepText = {
  ...STEP_TEXT[SetupStep.LinkOwnSand],
  action: "setup.invalidLink.action",
  detail: "setup.invalidLink.detail",
  next: "setup.invalidLink.next",
};

const ORDER = [
  SetupStep.LinkOwnSand,
  SetupStep.ConnectWorkSource,
  SetupStep.CreateRunnerProfile,
] as const;

function useSetupGuidance(organizationId: string) {
  const link = useQuery(OwnSandLinkService.method.getOwnSandLink, {
    organizationId,
  });
  const installations = useQuery(GitHubService.method.listGitHubInstallations, {
    organizationId,
    pageSize: 1,
  });
  const integrations = useQuery(
    AgentIntegrationService.method.listAgentIntegrations,
    { organizationId, pageSize: 1 },
  );
  const profiles = useQuery(RunnerProfileService.method.listRunnerProfiles, {
    organizationId,
    pageSize: 1,
  });
  const linkState = link.data && (link.data.link?.state ?? null);
  const step = nextSetupStep({
    linkState,
    hasGitHubInstallation:
      installations.data && installations.data.installations.length > 0,
    hasAgentIntegration:
      integrations.data && integrations.data.integrations.length > 0,
    hasRunnerProfile: profiles.data && profiles.data.profiles.length > 0,
  });
  if (step === undefined || step === SetupStep.Done) return undefined;
  const text: StepText =
    linkState === OwnSandLinkState.INVALID
      ? INVALID_LINK_TEXT
      : STEP_TEXT[step];
  return { step, text };
}

/** The setup checklist of the overview, while a step is not done. */
export function SetupGuide({ organizationId }: { organizationId: string }) {
  const { t } = useI18n();
  const guidance = useSetupGuidance(organizationId);
  if (guidance === undefined) return null;
  const nextIndex = ORDER.indexOf(guidance.step);
  return (
    <section className="section" aria-labelledby="setup-title">
      <h2 id="setup-title">{t("setup.title")}</h2>
      <ol className="setup-steps">
        {ORDER.map((step, index) => {
          const current = index === nextIndex;
          const text: StepText = current ? guidance.text : STEP_TEXT[step];
          const done = index < nextIndex;
          return (
            <li key={step} className="setup-step" data-done={done}>
              <span className="muted">
                {done
                  ? t("setup.stepDone")
                  : t("setup.stepNumber", { number: index + 1 })}
              </span>
              <span>
                <span className="setup-step-title">{t(text.title)}</span>
                {current ? (
                  <>
                    <br />
                    <span className="muted">{t(text.detail)}</span>
                    <br />
                    <Link to={orgPath(text.route, organizationId)}>
                      {t(text.action ?? text.title)}
                    </Link>
                  </>
                ) : null}
              </span>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

/**
 * Shows the next setup step when known, or the given empty-state text.
 */
export function SetupEmpty({
  organizationId,
  title,
  children,
}: {
  organizationId: string;
  title: string;
  children?: React.ReactNode;
}) {
  const { t } = useI18n();
  const guidance = useSetupGuidance(organizationId);
  if (guidance === undefined) {
    return <Empty title={title}>{children}</Empty>;
  }
  const { text } = guidance;
  const action = t(text.action ?? text.title);
  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      <p className="muted">
        {t(text.next)} {t(text.detail)}
      </p>
      <Link to={orgPath(text.route, organizationId)}>{action}</Link>
    </div>
  );
}
