// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { AgentIntegrationService } from "@delino/tide-protos/v1/agent_integration_pb";
import { GitHubService } from "@delino/tide-protos/v1/github_pb";
import { OwnSandLinkService } from "@delino/tide-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/tide-protos/v1/runner_profile_pb";
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
  linked: boolean | undefined;
  hasGitHubInstallation: boolean | undefined;
  hasAgentIntegration: boolean | undefined;
  hasRunnerProfile: boolean | undefined;
}

/** Returns the next step when its prerequisites are known. */
export function nextSetupStep(state: SetupState): SetupStep | undefined {
  if (state.linked === undefined) return undefined;
  if (!state.linked) return SetupStep.LinkOwnSand;
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

const STEP_TEXT: Record<
  Exclude<SetupStep, SetupStep.Done>,
  {
    title: string;
    detail: string;
    route: RouteId.Integrations | RouteId.Profiles;
  }
> = {
  [SetupStep.LinkOwnSand]: {
    title: "Link OwnSand",
    detail:
      "Link the OwnSand organization whose devices run your work, with an OwnSand organization API key.",
    route: RouteId.Integrations,
  },
  [SetupStep.ConnectWorkSource]: {
    title: "Connect GitHub or create an agent integration",
    detail:
      "Install the tide GitHub App for GitHub Actions jobs, or add a Claude Code, Cursor, OpenAI Codex or Devin integration for agent sessions.",
    route: RouteId.Integrations,
  },
  [SetupStep.CreateRunnerProfile]: {
    title: "Create a runner profile",
    detail:
      "A runner profile describes one kind of runner: its workload, guest OS, resources and priority.",
    route: RouteId.Profiles,
  },
};

const ORDER = [
  SetupStep.LinkOwnSand,
  SetupStep.ConnectWorkSource,
  SetupStep.CreateRunnerProfile,
] as const;

function useNextSetupStep(organizationId: string): SetupStep | undefined {
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
  return nextSetupStep({
    linked: link.data && link.data.link !== undefined,
    hasGitHubInstallation:
      installations.data && installations.data.installations.length > 0,
    hasAgentIntegration:
      integrations.data && integrations.data.integrations.length > 0,
    hasRunnerProfile: profiles.data && profiles.data.profiles.length > 0,
  });
}

/** The setup checklist of the overview, while a step is not done. */
export function SetupGuide({ organizationId }: { organizationId: string }) {
  const next = useNextSetupStep(organizationId);
  if (next === undefined || next === SetupStep.Done) return null;
  const nextIndex = ORDER.indexOf(next);
  return (
    <section className="section" aria-labelledby="setup-title">
      <h2 id="setup-title">Set up tide</h2>
      <ol className="setup-steps">
        {ORDER.map((step, index) => {
          const text = STEP_TEXT[step];
          const done = index < nextIndex;
          return (
            <li key={step} className="setup-step" data-done={done}>
              <span className="muted">{done ? "Done" : `${index + 1}.`}</span>
              <span>
                <span className="setup-step-title">{text.title}</span>
                {step === next ? (
                  <>
                    <br />
                    <span className="muted">{text.detail}</span>
                    <br />
                    <Link to={orgPath(text.route, organizationId)}>
                      {text.title}
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
  const next = useNextSetupStep(organizationId);
  if (next === undefined || next === SetupStep.Done) {
    return <Empty title={title}>{children}</Empty>;
  }
  const text = STEP_TEXT[next];
  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      <p className="muted">
        Next step: {text.title.toLowerCase()}. {text.detail}
      </p>
      <Link to={orgPath(text.route, organizationId)}>{text.title}</Link>
    </div>
  );
}
