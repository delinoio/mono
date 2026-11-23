// SPDX-License-Identifier: Apache-2.0
import {
  type CallOptions,
  type Client,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import {
  type AgentIntegration,
  AgentIntegrationService,
  type ListAgentIntegrationsRequestSchema,
} from "@delino/tide-protos/v1/agent_integration_pb";
import {
  type ApiKey,
  ApiKeyService,
  type ListApiKeysRequestSchema,
} from "@delino/tide-protos/v1/api_key_pb";
import {
  type AuditEvent,
  AuditService,
  type ListAuditEventsRequestSchema,
} from "@delino/tide-protos/v1/audit_pb";
import {
  type DevSession,
  type DevSessionProfile,
  DevSessionService,
  type ListDevSessionProfilesRequestSchema,
  type ListDevSessionsRequestSchema,
} from "@delino/tide-protos/v1/dev_session_pb";
import {
  type GitHubInstallation,
  GitHubService,
  type ListGitHubInstallationsRequestSchema,
} from "@delino/tide-protos/v1/github_pb";
import {
  type Invitation,
  type ListInvitationsRequestSchema,
  type ListMembersRequestSchema,
  type Member,
  MemberService,
} from "@delino/tide-protos/v1/member_pb";
import {
  type ListOrganizationsRequestSchema,
  type Organization,
  OrganizationService,
} from "@delino/tide-protos/v1/organization_pb";
import { OwnSandLinkService } from "@delino/tide-protos/v1/ownsand_link_pb";
import {
  type ListRunnerProfilesRequestSchema,
  type RunnerProfile,
  RunnerProfileService,
} from "@delino/tide-protos/v1/runner_profile_pb";
import {
  type ListRequestsRequestSchema,
  type ListRunnersRequestSchema,
  type Request,
  type Runner,
  WorkService,
} from "@delino/tide-protos/v1/work_pb";
import { type Clock, systemClock } from "./clock.ts";
import {
  createApiTransport,
  resolveApiOrigin,
  type TokenSource,
} from "./connection.ts";
import { TideConfigError } from "./errors.ts";
import {
  type ExecEvent,
  type ExecInput,
  type ExecResult,
  execDevSession,
  runDevSessionExec,
} from "./exec.ts";
import { type PageRequest, paginate } from "./pagination.ts";
import { UuidV7Generator } from "./uuidv7.ts";
import {
  type WatchQueueEvent,
  type WatchQueueOptions,
  watchQueue,
} from "./watch-queue.ts";

export interface TideClientOptions {
  /** API origin. Default: https://tide.api.delino.io. */
  origin?: string;
  /**
   * A Logto access token for the API resource https://tide.api.delino.io,
   * or a personal API key, or a function that returns one before each call.
   * Personal API keys work only on `DevSessionService`.
   */
  token: TokenSource;
  /** Fetch implementation. Default: the global fetch. */
  fetch?: typeof globalThis.fetch;
  /** Time source of the stream helpers. Default: the system clock. */
  clock?: Clock;
}

/**
 * Pagination iterators for every list RPC. Each takes the list request
 * without `page_token` and yields every item of every page.
 */
export interface TideIterators {
  organizations(
    request?: PageRequest<typeof ListOrganizationsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<Organization, void, undefined>;
  invitations(
    request: PageRequest<typeof ListInvitationsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<Invitation, void, undefined>;
  members(
    request: PageRequest<typeof ListMembersRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<Member, void, undefined>;
  gitHubInstallations(
    request: PageRequest<typeof ListGitHubInstallationsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<GitHubInstallation, void, undefined>;
  agentIntegrations(
    request: PageRequest<typeof ListAgentIntegrationsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<AgentIntegration, void, undefined>;
  runnerProfiles(
    request: PageRequest<typeof ListRunnerProfilesRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<RunnerProfile, void, undefined>;
  requests(
    request: PageRequest<typeof ListRequestsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<Request, void, undefined>;
  runners(
    request: PageRequest<typeof ListRunnersRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<Runner, void, undefined>;
  apiKeys(
    request: PageRequest<typeof ListApiKeysRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<ApiKey, void, undefined>;
  devSessionProfiles(
    request: PageRequest<typeof ListDevSessionProfilesRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<DevSessionProfile, void, undefined>;
  devSessions(
    request: PageRequest<typeof ListDevSessionsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<DevSession, void, undefined>;
  auditEvents(
    request: PageRequest<typeof ListAuditEventsRequestSchema>,
    options?: CallOptions,
  ): AsyncGenerator<AuditEvent, void, undefined>;
}

/** The service clients of `delino.tide.v1` on one transport. */
export interface TideServices {
  readonly organizations: Client<typeof OrganizationService>;
  readonly members: Client<typeof MemberService>;
  readonly ownSandLink: Client<typeof OwnSandLinkService>;
  readonly gitHub: Client<typeof GitHubService>;
  readonly agentIntegrations: Client<typeof AgentIntegrationService>;
  readonly runnerProfiles: Client<typeof RunnerProfileService>;
  readonly work: Client<typeof WorkService>;
  readonly apiKeys: Client<typeof ApiKeyService>;
  readonly devSessions: Client<typeof DevSessionService>;
  readonly audit: Client<typeof AuditService>;
}

/** Creates the clients of every public service on a transport. */
export function createTideServices(transport: Transport): TideServices {
  return {
    organizations: createClient(OrganizationService, transport),
    members: createClient(MemberService, transport),
    ownSandLink: createClient(OwnSandLinkService, transport),
    gitHub: createClient(GitHubService, transport),
    agentIntegrations: createClient(AgentIntegrationService, transport),
    runnerProfiles: createClient(RunnerProfileService, transport),
    work: createClient(WorkService, transport),
    apiKeys: createClient(ApiKeyService, transport),
    devSessions: createClient(DevSessionService, transport),
    audit: createClient(AuditService, transport),
  };
}

/** Creates the pagination iterators of the given service clients. */
export function createTideIterators(services: TideServices): TideIterators {
  const s = services;
  return {
    organizations: (request = {}, options) =>
      paginate(
        (r, o) => s.organizations.listOrganizations(r, o),
        request,
        (res) => res.organizations,
        options,
      ),
    invitations: (request, options) =>
      paginate(
        (r, o) => s.members.listInvitations(r, o),
        request,
        (res) => res.invitations,
        options,
      ),
    members: (request, options) =>
      paginate(
        (r, o) => s.members.listMembers(r, o),
        request,
        (res) => res.members,
        options,
      ),
    gitHubInstallations: (request, options) =>
      paginate(
        (r, o) => s.gitHub.listGitHubInstallations(r, o),
        request,
        (res) => res.installations,
        options,
      ),
    agentIntegrations: (request, options) =>
      paginate(
        (r, o) => s.agentIntegrations.listAgentIntegrations(r, o),
        request,
        (res) => res.integrations,
        options,
      ),
    runnerProfiles: (request, options) =>
      paginate(
        (r, o) => s.runnerProfiles.listRunnerProfiles(r, o),
        request,
        (res) => res.profiles,
        options,
      ),
    requests: (request, options) =>
      paginate(
        (r, o) => s.work.listRequests(r, o),
        request,
        (res) => res.requests,
        options,
      ),
    runners: (request, options) =>
      paginate(
        (r, o) => s.work.listRunners(r, o),
        request,
        (res) => res.runners,
        options,
      ),
    apiKeys: (request, options) =>
      paginate(
        (r, o) => s.apiKeys.listApiKeys(r, o),
        request,
        (res) => res.apiKeys,
        options,
      ),
    devSessionProfiles: (request, options) =>
      paginate(
        (r, o) => s.devSessions.listDevSessionProfiles(r, o),
        request,
        (res) => res.profiles,
        options,
      ),
    devSessions: (request, options) =>
      paginate(
        (r, o) => s.devSessions.listDevSessions(r, o),
        request,
        (res) => res.devSessions,
        options,
      ),
    auditEvents: (request, options) =>
      paginate(
        (r, o) => s.audit.listAuditEvents(r, o),
        request,
        (res) => res.events,
        options,
      ),
  };
}

/**
 * The tide client: a client of every public service of `delino.tide.v1`
 * with the generated request and response types, pagination iterators and
 * the `WatchQueue` and exec helpers. RPC errors are `ConnectError` values
 * with the server's Connect code. The client does not log.
 */
export class TideClient implements TideServices {
  readonly origin: string;
  readonly organizations: Client<typeof OrganizationService>;
  readonly members: Client<typeof MemberService>;
  readonly ownSandLink: Client<typeof OwnSandLinkService>;
  readonly gitHub: Client<typeof GitHubService>;
  readonly agentIntegrations: Client<typeof AgentIntegrationService>;
  readonly runnerProfiles: Client<typeof RunnerProfileService>;
  readonly work: Client<typeof WorkService>;
  readonly apiKeys: Client<typeof ApiKeyService>;
  readonly devSessions: Client<typeof DevSessionService>;
  readonly audit: Client<typeof AuditService>;
  /** Pagination iterators for every list RPC. */
  readonly iterate: TideIterators;
  readonly #clock: Clock;
  readonly #ids: UuidV7Generator;

  constructor(options: TideClientOptions) {
    if (options === undefined || options.token === undefined) {
      throw new TideConfigError("token is required");
    }
    this.origin = resolveApiOrigin(options.origin);
    this.#clock = options.clock ?? systemClock;
    this.#ids = new UuidV7Generator(() => this.#clock.now());
    const transport = createApiTransport({
      origin: this.origin,
      token: options.token,
      fetch: options.fetch ?? globalThis.fetch.bind(globalThis),
    });
    const services = createTideServices(transport);
    this.organizations = services.organizations;
    this.members = services.members;
    this.ownSandLink = services.ownSandLink;
    this.gitHub = services.gitHub;
    this.agentIntegrations = services.agentIntegrations;
    this.runnerProfiles = services.runnerProfiles;
    this.work = services.work;
    this.apiKeys = services.apiKeys;
    this.devSessions = services.devSessions;
    this.audit = services.audit;
    this.iterate = createTideIterators(services);
  }

  /** Streams the queue and reconnects; see `watchQueue`. */
  watchQueue(
    options: Omit<WatchQueueOptions, "clock">,
  ): AsyncGenerator<WatchQueueEvent, void, undefined> {
    return watchQueue(this.work, { ...options, clock: this.#clock });
  }

  /** Runs a command in a dev session and streams it; see `execDevSession`. */
  execDevSession(input: ExecInput): AsyncGenerator<ExecEvent, void, undefined> {
    return execDevSession(this.devSessions, input, {
      clock: this.#clock,
      ids: this.#ids,
    });
  }

  /** Runs a command in a dev session and collects its output. */
  runDevSessionExec(input: ExecInput): Promise<ExecResult> {
    return runDevSessionExec(this.devSessions, input, {
      clock: this.#clock,
      ids: this.#ids,
    });
  }
}
