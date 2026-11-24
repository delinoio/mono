// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type Duration,
  DurationSchema,
  type FieldMask,
  type Timestamp,
  timestampFromMs,
} from "@bufbuild/protobuf/wkt";
import {
  Code,
  ConnectError,
  createRouterTransport,
  type HandlerContext,
  type Transport,
} from "@connectrpc/connect";
import {
  AgentCredential,
  type AgentIntegration,
  AgentIntegrationSchema,
  AgentIntegrationService,
  type CreateAgentIntegrationRequest,
} from "@delino/tide-protos/v1/agent_integration_pb";
import {
  type ApiKey,
  ApiKeySchema,
  ApiKeyService,
} from "@delino/tide-protos/v1/api_key_pb";
import {
  AuditAction,
  type AuditEvent,
  AuditEventSchema,
  AuditService,
  AuditTargetKind,
} from "@delino/tide-protos/v1/audit_pb";
import {
  AgentVendor,
  ApiKeyState,
  DeviceState,
  GitHubInstallationState,
  GuestOs,
  InvitationState,
  NetworkMode,
  OwnSandLinkState,
  Priority,
  RequestEndReason,
  RequestState,
  Role,
  RunnerEndReason,
  ResourcesSchema,
  RunnerState,
  WorkloadKind,
} from "@delino/tide-protos/v1/common_pb";
import {
  type DevSession,
  DevSessionSchema,
  DevSessionService,
} from "@delino/tide-protos/v1/dev_session_pb";
import {
  type GitHubInstallation,
  GitHubInstallationSchema,
  GitHubService,
} from "@delino/tide-protos/v1/github_pb";
import {
  type Invitation,
  InvitationSchema,
  type Member,
  MemberSchema,
  MemberService,
} from "@delino/tide-protos/v1/member_pb";
import {
  type Organization,
  OrganizationSchema,
  OrganizationService,
} from "@delino/tide-protos/v1/organization_pb";
import {
  type OwnSandLink,
  OwnSandLinkSchema,
  OwnSandLinkService,
} from "@delino/tide-protos/v1/ownsand_link_pb";
import {
  type RunnerProfile,
  RunnerProfileSchema,
  RunnerProfileService,
} from "@delino/tide-protos/v1/runner_profile_pb";
import {
  type DeviceCapacity,
  DeviceCapacitySchema,
  type Request,
  RequestSchema,
  type Runner,
  RunnerSchema,
  type WatchQueueResponseSchema,
  WorkService,
} from "@delino/tide-protos/v1/work_pb";
import { TERMINAL_REQUEST_STATES, TERMINAL_RUNNER_STATES } from "../format.ts";
import { uuidV7 } from "../uuid.ts";

/**
 * An in-browser fake of the tide control plane for the development server
 * (`PUBLIC_TIDE_MOCK_API=true`), the unit tests and the end-to-end suite. It
 * applies the role matrix, so hidden actions are also rejected. Production
 * builds never include it.
 */

export const MOCK_USER = "mock-user-owner";
/** A seeded invitation secret that joins the Contoso organization. */
export const MOCK_INVITATION_SECRET = "tdi_mock_contoso_invitation";
/** The GitHub numeric installation ID that the mock GitHub callback uses. */
export const MOCK_GITHUB_INSTALLATION_ID = 51_000_001n;
/** The origin of the mock OpenAI webhook URLs. */
export const MOCK_API_ORIGIN = "https://tide.api.delino.io";

export const MOCK_IDS = {
  /** Owner; fully set up. */
  acme: "0193b000-0000-7000-8000-000000000001",
  /** Member; the OwnSand link is INVALID. */
  northwind: "0193b000-0000-7000-8000-000000000002",
  /** Joined with the seeded invitation. */
  contoso: "0193b000-0000-7000-8000-000000000003",
  /** Admin; nothing is set up yet. */
  globex: "0193b000-0000-7000-8000-000000000004",
  ownsandAcme: "0193a000-0000-7000-8000-000000000001",
  ownsandNorthwind: "0193a000-0000-7000-8000-000000000002",
  linuxProfile: "0193b000-0000-7000-8000-0000000000b1",
  codexProfile: "0193b000-0000-7000-8000-0000000000b2",
  devProfile: "0193b000-0000-7000-8000-0000000000b3",
  codexIntegration: "0193b000-0000-7000-8000-0000000000c1",
  claudeIntegration: "0193b000-0000-7000-8000-0000000000c2",
  installation: "0193b000-0000-7000-8000-0000000000d1",
  queuedJob: "0193b000-0000-7000-8000-0000000000e1",
  queuedAgent: "0193b000-0000-7000-8000-0000000000e2",
  queuedDev: "0193b000-0000-7000-8000-0000000000e3",
  runningJob: "0193b000-0000-7000-8000-0000000000e4",
  endedJob: "0193b000-0000-7000-8000-0000000000e5",
  busyRunner: "0193b000-0000-7000-8000-0000000000f1",
  warmRunner: "0193b000-0000-7000-8000-0000000000f2",
  deletedRunner: "0193b000-0000-7000-8000-0000000000f3",
  devSessionOwn: "0193b000-0000-7000-8000-0000000000a1",
  devSessionOther: "0193b000-0000-7000-8000-0000000000a2",
  apiKey: "0193b000-0000-7000-8000-000000000091",
  linuxDevice: "0193a000-0000-7000-8000-0000000000d1",
  macDevice: "0193a000-0000-7000-8000-0000000000d2",
  windowsDevice: "0193a000-0000-7000-8000-0000000000d3",
} as const;

const OTHER_ADMIN = "mock-user-ada";
const OTHER_MEMBER = "mock-user-lin";

const now = () => Date.now();
const ts = (offsetSeconds = 0): Timestamp =>
  timestampFromMs(now() + offsetSeconds * 1000);
const dur = (seconds: number): Duration =>
  create(DurationSchema, { seconds: BigInt(seconds) });

interface OrgState {
  organization: Organization;
  members: Member[];
  invitations: (Invitation & { secret: string })[];
  link: OwnSandLink | undefined;
  installations: GitHubInstallation[];
  integrations: AgentIntegration[];
  profiles: RunnerProfile[];
  requests: Request[];
  runners: Runner[];
  devices: DeviceCapacity[];
  devSessions: DevSession[];
  apiKeys: (ApiKey & { userId: string })[];
  audit: AuditEvent[];
}

function member(
  organizationId: string,
  userId: string,
  role: Role,
  displayName: string,
): Member {
  return create(MemberSchema, {
    organizationId,
    userId,
    role,
    displayName,
    joinTime: ts(-30 * 86400),
  });
}

function emptyOrg(organizationId: string, name: string): OrgState {
  return {
    organization: create(OrganizationSchema, {
      organizationId,
      name,
      createTime: ts(),
      settings: {},
    }),
    members: [],
    invitations: [],
    link: undefined,
    installations: [],
    integrations: [],
    profiles: [],
    requests: [],
    runners: [],
    devices: [],
    devSessions: [],
    apiKeys: [],
    audit: [],
  };
}

function profile(
  organizationId: string,
  init: MessageInitShape<typeof RunnerProfileSchema>,
): RunnerProfile {
  const p = create(RunnerProfileSchema, init);
  p.organizationId = organizationId;
  p.networkMode = NetworkMode.OUTBOUND;
  p.resources ??= create(ResourcesSchema, {
    vcpuCount: 4,
    memoryMib: 8192n,
    diskGib: 64n,
  });
  p.idleTtl ??= dur(1800);
  p.maxTtl ??= dur(6 * 3600);
  p.maxQueueWait ??= dur(3600);
  p.createTime ??= ts(-7 * 86400);
  p.updateTime ??= p.createTime;
  if (p.workloadKind === WorkloadKind.GITHUB_JOB)
    p.githubLabel = `tide-${p.name}`;
  return p;
}

function seed(userId: string): Map<string, OrgState> {
  const orgs = new Map<string, OrgState>();

  const acme = emptyOrg(MOCK_IDS.acme, "Acme Robotics");
  acme.organization.settings = {
    $typeName: "delino.tide.v1.OrganizationSettings",
    maxDevSessionsPerUser: 2,
  };
  acme.members.push(
    member(MOCK_IDS.acme, userId, Role.OWNER, "Mina Kim"),
    member(MOCK_IDS.acme, OTHER_ADMIN, Role.ADMIN, "Ada Park"),
    member(MOCK_IDS.acme, OTHER_MEMBER, Role.MEMBER, ""),
  );
  acme.link = create(OwnSandLinkSchema, {
    organizationId: MOCK_IDS.acme,
    ownsandOrganizationId: MOCK_IDS.ownsandAcme,
    state: OwnSandLinkState.ACTIVE,
    credentialSet: true,
    credentialSetAt: ts(-10 * 86400),
    createTime: ts(-10 * 86400),
  });
  acme.installations.push(
    create(GitHubInstallationSchema, {
      installationId: MOCK_IDS.installation,
      organizationId: MOCK_IDS.acme,
      githubInstallationId: 41_000_001n,
      githubAccountLogin: "acme-robotics",
      githubAccountId: 9_000_001n,
      state: GitHubInstallationState.ACTIVE,
      githubRunnerGroupId: 1n,
      runnerGroupName: "Default",
      createTime: ts(-9 * 86400),
    }),
  );
  acme.integrations.push(
    create(AgentIntegrationSchema, {
      agentIntegrationId: MOCK_IDS.codexIntegration,
      organizationId: MOCK_IDS.acme,
      vendor: AgentVendor.OPENAI_CODEX,
      name: "codex-prod",
      settings: { case: "openaiCodex", value: {} },
      credentials: [
        {
          credential: AgentCredential.ENVIRONMENT_KEY,
          credentialSet: true,
          credentialSetAt: ts(-8 * 86400),
        },
        {
          credential: AgentCredential.WEBHOOK_SECRET,
          credentialSet: true,
          credentialSetAt: ts(-8 * 86400),
        },
        {
          credential: AgentCredential.API_KEY,
          credentialSet: true,
          credentialSetAt: ts(-8 * 86400),
        },
      ],
      webhookUrl: `${MOCK_API_ORIGIN}/webhooks/openai/${MOCK_IDS.codexIntegration}`,
      createTime: ts(-8 * 86400),
      updateTime: ts(-8 * 86400),
    }),
    create(AgentIntegrationSchema, {
      agentIntegrationId: MOCK_IDS.claudeIntegration,
      organizationId: MOCK_IDS.acme,
      vendor: AgentVendor.CLAUDE_CODE,
      name: "claude-team",
      settings: { case: "claudeCode", value: {} },
      credentials: [
        {
          credential: AgentCredential.ENVIRONMENT_KEY,
          credentialSet: true,
          credentialSetAt: ts(-6 * 86400),
        },
      ],
      createTime: ts(-6 * 86400),
      updateTime: ts(-6 * 86400),
    }),
  );
  acme.profiles.push(
    profile(MOCK_IDS.acme, {
      runnerProfileId: MOCK_IDS.linuxProfile,
      name: "linux-build",
      workloadKind: WorkloadKind.GITHUB_JOB,
      guestOs: GuestOs.LINUX,
      source: { case: "baseImage", value: GuestOs.LINUX },
      priority: Priority.HIGH,
      warmCount: 1,
    }),
    profile(MOCK_IDS.acme, {
      runnerProfileId: MOCK_IDS.codexProfile,
      name: "codex-linux",
      workloadKind: WorkloadKind.AGENT_SESSION,
      agentIntegrationId: MOCK_IDS.codexIntegration,
      guestOs: GuestOs.LINUX,
      source: { case: "baseImage", value: GuestOs.LINUX },
      priority: Priority.NORMAL,
      maxQueueWait: dur(300),
    }),
    profile(MOCK_IDS.acme, {
      runnerProfileId: MOCK_IDS.devProfile,
      name: "windows-dev",
      workloadKind: WorkloadKind.DEV_SESSION,
      workspacePath: "C:\\work",
      guestOs: GuestOs.WINDOWS,
      source: {
        case: "ownsandTemplateId",
        value: "0193a000-0000-7000-8000-0000000000c9",
      },
      priority: Priority.NORMAL,
    }),
  );
  acme.requests.push(
    create(RequestSchema, {
      requestId: MOCK_IDS.queuedJob,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.linuxProfile,
      kind: WorkloadKind.GITHUB_JOB,
      sourceKey: "31000001",
      state: RequestState.QUEUED,
      priority: Priority.HIGH,
      queuedAt: ts(-240),
      githubJob: {
        githubInstallationId: 41_000_001n,
        repositoryFullName: "acme-robotics/firmware",
        githubWorkflowRunId: 77_001n,
        jobName: "test (ubuntu)",
        githubJobId: 31_000_001n,
      },
    }),
    create(RequestSchema, {
      requestId: MOCK_IDS.queuedAgent,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.codexProfile,
      kind: WorkloadKind.AGENT_SESSION,
      sourceKey: "sess_codex_4711",
      state: RequestState.QUEUED,
      priority: Priority.NORMAL,
      queuedAt: ts(-600),
    }),
    create(RequestSchema, {
      requestId: MOCK_IDS.queuedDev,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.devProfile,
      kind: WorkloadKind.DEV_SESSION,
      state: RequestState.QUEUED,
      priority: Priority.NORMAL,
      queuedAt: ts(-60),
      ownerUserId: userId,
    }),
    create(RequestSchema, {
      requestId: MOCK_IDS.runningJob,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.linuxProfile,
      kind: WorkloadKind.GITHUB_JOB,
      sourceKey: "31000000",
      state: RequestState.RUNNING,
      priority: Priority.HIGH,
      queuedAt: ts(-1200),
      startedAt: ts(-1100),
      runnerId: MOCK_IDS.busyRunner,
      githubJob: {
        githubInstallationId: 41_000_001n,
        repositoryFullName: "acme-robotics/firmware",
        githubWorkflowRunId: 77_000n,
        jobName: "build (ubuntu)",
        githubJobId: 31_000_000n,
      },
    }),
    create(RequestSchema, {
      requestId: MOCK_IDS.endedJob,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.linuxProfile,
      kind: WorkloadKind.GITHUB_JOB,
      sourceKey: "30999999",
      state: RequestState.ENDED,
      endReason: RequestEndReason.COMPLETED,
      priority: Priority.HIGH,
      queuedAt: ts(-7200),
      startedAt: ts(-7000),
      endedAt: ts(-6000),
      runnerId: MOCK_IDS.deletedRunner,
      githubJob: {
        githubInstallationId: 41_000_001n,
        repositoryFullName: "acme-robotics/site",
        githubWorkflowRunId: 76_000n,
        jobName: "deploy",
        githubJobId: 30_999_999n,
      },
    }),
  );
  acme.runners.push(
    create(RunnerSchema, {
      runnerId: MOCK_IDS.busyRunner,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.linuxProfile,
      kind: WorkloadKind.GITHUB_JOB,
      state: RunnerState.BUSY,
      ownsandSandboxId: "0193a000-0000-7000-8000-0000000000a1",
      ownsandDeviceId: MOCK_IDS.linuxDevice,
      requestId: MOCK_IDS.runningJob,
      githubInstallationId: 41_000_001n,
      createdAt: ts(-1150),
      guestOs: GuestOs.LINUX,
    }),
    create(RunnerSchema, {
      runnerId: MOCK_IDS.warmRunner,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.linuxProfile,
      kind: WorkloadKind.GITHUB_JOB,
      state: RunnerState.WARM,
      ownsandSandboxId: "0193a000-0000-7000-8000-0000000000a2",
      ownsandDeviceId: MOCK_IDS.linuxDevice,
      githubInstallationId: 41_000_001n,
      createdAt: ts(-900),
      guestOs: GuestOs.LINUX,
    }),
    create(RunnerSchema, {
      runnerId: MOCK_IDS.deletedRunner,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.linuxProfile,
      kind: WorkloadKind.GITHUB_JOB,
      state: RunnerState.DELETED,
      endReason: RunnerEndReason.WORK_DONE,
      ownsandSandboxId: "0193a000-0000-7000-8000-0000000000a3",
      ownsandDeviceId: MOCK_IDS.linuxDevice,
      requestId: MOCK_IDS.endedJob,
      githubInstallationId: 41_000_001n,
      createdAt: ts(-7100),
      endedAt: ts(-5900),
      guestOs: GuestOs.LINUX,
    }),
  );
  acme.devices.push(
    create(DeviceCapacitySchema, {
      ownsandDeviceId: MOCK_IDS.linuxDevice,
      name: "build-box",
      state: DeviceState.ONLINE,
      hostOs: "linux",
      arch: "amd64",
      limits: {
        vcpuCount: 16,
        memoryMib: 65536n,
        diskGib: 1000n,
        vcpuOvercommitPercent: 150,
      },
      effectiveVcpuLimit: 24n,
      allocation: { vcpuCount: 8n, memoryMib: 16384n, diskGib: 128n },
      runners: [
        { state: RunnerState.BUSY, count: 1 },
        { state: RunnerState.WARM, count: 1 },
      ],
    }),
    create(DeviceCapacitySchema, {
      ownsandDeviceId: MOCK_IDS.macDevice,
      name: "studio-mac",
      state: DeviceState.ONLINE,
      hostOs: "macos",
      arch: "arm64",
      limits: {
        vcpuCount: 12,
        memoryMib: 65536n,
        diskGib: 900n,
        vcpuOvercommitPercent: 100,
      },
      effectiveVcpuLimit: 12n,
      allocation: { vcpuCount: 0n, memoryMib: 0n, diskGib: 0n },
    }),
    create(DeviceCapacitySchema, {
      ownsandDeviceId: MOCK_IDS.windowsDevice,
      name: "win-desk",
      state: DeviceState.OFFLINE,
      hostOs: "windows",
      arch: "amd64",
      limits: {
        vcpuCount: 8,
        memoryMib: 32768n,
        diskGib: 500n,
        vcpuOvercommitPercent: 100,
      },
      effectiveVcpuLimit: 8n,
      allocation: { vcpuCount: 0n, memoryMib: 0n, diskGib: 0n },
    }),
  );
  acme.devSessions.push(
    create(DevSessionSchema, {
      devSessionId: MOCK_IDS.queuedDev,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.devProfile,
      runnerProfileName: "windows-dev",
      ownerUserId: userId,
      label: "msvc build",
      guestOs: GuestOs.WINDOWS,
      workspacePath: "C:\\work",
      state: RequestState.QUEUED,
      queuedAt: ts(-60),
    }),
    create(DevSessionSchema, {
      devSessionId: MOCK_IDS.devSessionOther,
      organizationId: MOCK_IDS.acme,
      runnerProfileId: MOCK_IDS.devProfile,
      runnerProfileName: "windows-dev",
      ownerUserId: OTHER_MEMBER,
      label: "",
      guestOs: GuestOs.WINDOWS,
      workspacePath: "C:\\work",
      state: RequestState.ENDED,
      endReason: RequestEndReason.SESSION_TTL_EXPIRED,
      queuedAt: ts(-86400),
      startedAt: ts(-86000),
      endedAt: ts(-60000),
    }),
  );
  acme.apiKeys.push(
    Object.assign(
      create(ApiKeySchema, {
        apiKeyId: MOCK_IDS.apiKey,
        organizationId: MOCK_IDS.acme,
        name: "claude-on-laptop",
        displayPrefix: "tide_k3Jd",
        state: ApiKeyState.ACTIVE,
        createdAt: ts(-5 * 86400),
        expiresAt: ts(85 * 86400),
        lastUsedAt: ts(-3600),
      }),
      { userId },
    ),
  );
  acme.audit.push(
    create(AuditEventSchema, {
      auditEventId: uuidV7(),
      organizationId: MOCK_IDS.acme,
      action: AuditAction.OWNSAND_LINK_SET,
      actorUserId: userId,
      targetKind: AuditTargetKind.OWNSAND_LINK,
      targetId: MOCK_IDS.acme,
      createTime: ts(-10 * 86400),
    }),
    create(AuditEventSchema, {
      auditEventId: uuidV7(),
      organizationId: MOCK_IDS.acme,
      action: AuditAction.DEV_SESSION_CREATED,
      actorUserId: userId,
      actorApiKeyId: MOCK_IDS.apiKey,
      targetKind: AuditTargetKind.DEV_SESSION,
      targetId: MOCK_IDS.queuedDev,
      subjectUserId: userId,
      createTime: ts(-60),
    }),
  );
  orgs.set(MOCK_IDS.acme, acme);

  const northwind = emptyOrg(MOCK_IDS.northwind, "Northwind Labs");
  northwind.members.push(
    member(MOCK_IDS.northwind, OTHER_ADMIN, Role.OWNER, "Ada Park"),
    member(MOCK_IDS.northwind, userId, Role.MEMBER, "Mina Kim"),
  );
  northwind.link = create(OwnSandLinkSchema, {
    organizationId: MOCK_IDS.northwind,
    ownsandOrganizationId: MOCK_IDS.ownsandNorthwind,
    state: OwnSandLinkState.INVALID,
    credentialSet: true,
    credentialSetAt: ts(-40 * 86400),
    createTime: ts(-40 * 86400),
  });
  northwind.profiles.push(
    profile(MOCK_IDS.northwind, {
      runnerProfileId: uuidV7(),
      name: "mac-build",
      workloadKind: WorkloadKind.GITHUB_JOB,
      guestOs: GuestOs.MACOS,
      source: { case: "baseImage", value: GuestOs.MACOS },
      priority: Priority.HIGH,
    }),
  );
  northwind.installations.push(
    create(GitHubInstallationSchema, {
      installationId: uuidV7(),
      organizationId: MOCK_IDS.northwind,
      githubInstallationId: 41_000_002n,
      githubAccountLogin: "northwind",
      state: GitHubInstallationState.ACTIVE,
      runnerGroupName: "Default",
      createTime: ts(-40 * 86400),
    }),
  );
  orgs.set(MOCK_IDS.northwind, northwind);

  const contoso = emptyOrg(MOCK_IDS.contoso, "Contoso");
  contoso.members.push(
    member(MOCK_IDS.contoso, OTHER_ADMIN, Role.OWNER, "Ada Park"),
  );
  contoso.invitations.push(
    Object.assign(
      create(InvitationSchema, {
        invitationId: uuidV7(),
        organizationId: MOCK_IDS.contoso,
        role: Role.MEMBER,
        state: InvitationState.PENDING,
        createdByUserId: OTHER_ADMIN,
        createTime: ts(-3600),
        expireTime: ts(6 * 86400),
        secretPrefix: "tdi_mock",
      }),
      { secret: MOCK_INVITATION_SECRET },
    ),
  );
  orgs.set(MOCK_IDS.contoso, contoso);

  const globex = emptyOrg(MOCK_IDS.globex, "Globex");
  globex.members.push(
    member(MOCK_IDS.globex, OTHER_ADMIN, Role.OWNER, "Ada Park"),
    member(MOCK_IDS.globex, userId, Role.ADMIN, "Mina Kim"),
  );
  orgs.set(MOCK_IDS.globex, globex);
  return orgs;
}

/** Serves one page of a list with numeric offsets as page tokens. */
function page<T>(
  items: readonly T[],
  req: { pageSize: number; pageToken: string },
): { items: T[]; nextPageToken: string } {
  if (req.pageSize < 0 || req.pageSize > 200) {
    throw new ConnectError(
      "page_size must be at most 200",
      Code.InvalidArgument,
    );
  }
  const size = req.pageSize === 0 ? 50 : req.pageSize;
  const start = req.pageToken === "" ? 0 : Number(req.pageToken);
  if (!Number.isInteger(start) || start < 0) {
    throw new ConnectError("invalid page_token", Code.InvalidArgument);
  }
  const slice = items.slice(start, start + size);
  const next = start + size < items.length ? String(start + size) : "";
  return { items: slice, nextPageToken: next };
}

function hasPath(mask: FieldMask | undefined, path: string): boolean {
  return mask?.paths.includes(path) ?? false;
}

export interface MockTransportOptions {
  /** The signed-in user. Default: `MOCK_USER`. */
  userId?: string;
  /**
   * Ends each WatchQueue stream after its snapshot and this many
   * milliseconds, like the control plane before 14 minutes. Default: the
   * stream stays open until the client cancels it.
   */
  watchQueueEndsAfterMs?: number;
}

/** A queue watcher of one organization. */
type Watcher = (
  event: MessageInitShape<typeof WatchQueueResponseSchema>,
) => void;

export function createMockTransport(
  options: MockTransportOptions = {},
): Transport {
  const userId = options.userId ?? MOCK_USER;
  const orgs = seed(userId);
  const watchers = new Map<string, Set<Watcher>>();
  const issuedStates = new Map<
    string,
    { organizationId: string; expiresAtMs: number }
  >();

  const membership = (orgId: string): { org: OrgState; role: Role } => {
    const org = orgs.get(orgId);
    const m = org?.members.find((x) => x.userId === userId);
    if (org === undefined || m === undefined) {
      throw new ConnectError("organization not found", Code.NotFound);
    }
    return { org, role: m.role };
  };
  const requireAdmin = (role: Role) => {
    if (role !== Role.OWNER && role !== Role.ADMIN) {
      throw new ConnectError("needs the admin role", Code.PermissionDenied);
    }
  };
  const requireOwner = (role: Role) => {
    if (role !== Role.OWNER) {
      throw new ConnectError("needs the owner role", Code.PermissionDenied);
    }
  };
  const withRole = (org: OrgState, role: Role) =>
    create(OrganizationSchema, { ...org.organization, callerRole: role });
  const audit = (
    org: OrgState,
    action: AuditAction,
    targetKind: AuditTargetKind,
    targetId: string,
    subjectUserId = "",
  ) => {
    org.audit.unshift(
      create(AuditEventSchema, {
        auditEventId: uuidV7(),
        organizationId: org.organization.organizationId,
        action,
        actorUserId: userId,
        targetKind,
        targetId,
        subjectUserId,
        createTime: ts(),
      }),
    );
  };
  const publish = (
    org: OrgState,
    event: MessageInitShape<typeof WatchQueueResponseSchema>,
  ) => {
    for (const w of watchers.get(org.organization.organizationId) ?? [])
      w(event);
  };
  const secret = (prefix: string) =>
    `${prefix}_${Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) =>
      b.toString(16).padStart(2, "0"),
    ).join("")}`;
  const findProfile = (org: OrgState, id: string) => {
    const p = org.profiles.find((x) => x.runnerProfileId === id);
    if (p === undefined)
      throw new ConnectError("runner profile not found", Code.NotFound);
    return p;
  };
  const findIntegration = (org: OrgState, id: string) => {
    const i = org.integrations.find((x) => x.agentIntegrationId === id);
    if (i === undefined)
      throw new ConnectError("agent integration not found", Code.NotFound);
    return i;
  };
  const findInstallation = (org: OrgState, id: string) => {
    const i = org.installations.find((x) => x.installationId === id);
    if (i === undefined)
      throw new ConnectError("installation not found", Code.NotFound);
    return i;
  };
  const endRequest = (
    org: OrgState,
    r: Request,
    state: RequestState,
    reason: RequestEndReason,
  ) => {
    r.state = state;
    r.endReason = reason;
    r.endedAt = ts();
    const session = org.devSessions.find((s) => s.devSessionId === r.requestId);
    if (session !== undefined) {
      session.state = state;
      session.endReason = reason;
      session.endedAt = r.endedAt;
    }
    publish(org, { event: { case: "request", value: r } });
  };
  const endRunner = (org: OrgState, r: Runner, reason: RunnerEndReason) => {
    r.state = RunnerState.DELETED;
    r.endReason = reason;
    r.endedAt = ts();
    publish(org, { event: { case: "runner", value: r } });
  };
  const credentialStates = (
    vendor: AgentVendor,
    previous: AgentIntegration["credentials"],
    req: Pick<CreateAgentIntegrationRequest, "credentials">,
  ): AgentIntegration["credentials"] => {
    const supplied: [AgentCredential, string][] = [];
    const c = req.credentials;
    switch (c.case) {
      case "claudeCodeCredentials":
        supplied.push([
          AgentCredential.ENVIRONMENT_KEY,
          c.value.environmentKey,
        ]);
        break;
      case "cursorCredentials":
        supplied.push([
          AgentCredential.SERVICE_ACCOUNT_API_KEY,
          c.value.serviceAccountApiKey,
        ]);
        break;
      case "openaiCodexCredentials":
        supplied.push(
          [AgentCredential.ENVIRONMENT_KEY, c.value.environmentKey],
          [AgentCredential.WEBHOOK_SECRET, c.value.webhookSecret],
          [AgentCredential.API_KEY, c.value.apiKey],
        );
        break;
      case "devinCredentials":
        supplied.push(
          [AgentCredential.API_TOKEN, c.value.apiToken],
          [AgentCredential.OUTPOST_TOKEN, c.value.outpostToken],
        );
        break;
    }
    const expected = VENDOR_CREDENTIAL_CASE[vendor];
    if (c.case !== undefined && c.case !== expected) {
      throw new ConnectError(
        "credentials do not match the vendor",
        Code.InvalidArgument,
      );
    }
    return supplied.map(([credential, value]) => {
      const old = previous.find((p) => p.credential === credential);
      if (value !== "") {
        return {
          $typeName: "delino.tide.v1.AgentCredentialState",
          credential,
          credentialSet: true,
          credentialSetAt: ts(),
        };
      }
      if (old === undefined || !old.credentialSet) {
        throw new ConnectError(
          "every credential of the vendor is required",
          Code.InvalidArgument,
        );
      }
      return old;
    });
  };

  return createRouterTransport((router) => {
    router.service(OrganizationService, {
      createOrganization(req) {
        const name = req.name.trim();
        if (name === "" || name.length > 100)
          throw new ConnectError(
            "name must have 1 to 100 characters",
            Code.InvalidArgument,
          );
        const id = uuidV7();
        const org = emptyOrg(id, name);
        org.members.push(member(id, userId, Role.OWNER, "Mina Kim"));
        orgs.set(id, org);
        audit(
          org,
          AuditAction.ORGANIZATION_CREATED,
          AuditTargetKind.ORGANIZATION,
          id,
        );
        return { organization: withRole(org, Role.OWNER) };
      },
      getOrganization(req) {
        const { org, role } = membership(req.organizationId);
        return { organization: withRole(org, role) };
      },
      listOrganizations(req) {
        const mine = [...orgs.values()].flatMap((org) => {
          const m = org.members.find((x) => x.userId === userId);
          return m === undefined ? [] : [withRole(org, m.role)];
        });
        const p = page(mine, req);
        return { organizations: p.items, nextPageToken: p.nextPageToken };
      },
      deleteOrganization(req) {
        const { role } = membership(req.organizationId);
        requireOwner(role);
        orgs.delete(req.organizationId);
        return {};
      },
      transferOwnership(req) {
        const { org, role } = membership(req.organizationId);
        requireOwner(role);
        const target = org.members.find((m) => m.userId === req.userId);
        if (target === undefined || target.userId === userId)
          throw new ConnectError("not a member", Code.FailedPrecondition);
        target.role = Role.OWNER;
        org.members.find((m) => m.userId === userId)!.role = Role.ADMIN;
        audit(
          org,
          AuditAction.OWNERSHIP_TRANSFERRED,
          AuditTargetKind.ORGANIZATION,
          req.organizationId,
          req.userId,
        );
        return {};
      },
      updateOrganizationSettings(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (hasPath(req.updateMask, "max_dev_sessions_per_user")) {
          const value = req.settings?.maxDevSessionsPerUser;
          if (value !== undefined && value < 1)
            throw new ConnectError(
              "max_dev_sessions_per_user must be greater than zero",
              Code.InvalidArgument,
            );
          org.organization.settings = {
            $typeName: "delino.tide.v1.OrganizationSettings",
            maxDevSessionsPerUser: value,
          };
        }
        audit(
          org,
          AuditAction.ORGANIZATION_SETTINGS_CHANGED,
          AuditTargetKind.ORGANIZATION,
          req.organizationId,
        );
        return { organization: withRole(org, role) };
      },
    });

    router.service(MemberService, {
      createInvitation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (req.role !== Role.ADMIN && req.role !== Role.MEMBER)
          throw new ConnectError(
            "role must be ADMIN or MEMBER",
            Code.InvalidArgument,
          );
        const value = secret("tdi");
        const invitation = Object.assign(
          create(InvitationSchema, {
            invitationId: uuidV7(),
            organizationId: req.organizationId,
            role: req.role,
            state: InvitationState.PENDING,
            createdByUserId: userId,
            createTime: ts(),
            expireTime: ts(7 * 86400),
            secretPrefix: value.slice(0, 8),
          }),
          { secret: value },
        );
        org.invitations.push(invitation);
        audit(
          org,
          AuditAction.INVITATION_CREATED,
          AuditTargetKind.INVITATION,
          invitation.invitationId,
        );
        const { secret: _secret, ...rest } = invitation;
        return { invitation: rest, invitationSecret: value };
      },
      listInvitations(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const p = page(org.invitations, req);
        return {
          invitations: p.items.map(({ secret: _s, ...rest }) => rest),
          nextPageToken: p.nextPageToken,
        };
      },
      revokeInvitation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const i = org.invitations.find(
          (x) => x.invitationId === req.invitationId,
        );
        if (i === undefined)
          throw new ConnectError("invitation not found", Code.NotFound);
        i.state = InvitationState.REVOKED;
        audit(
          org,
          AuditAction.INVITATION_REVOKED,
          AuditTargetKind.INVITATION,
          i.invitationId,
        );
        const { secret: _s, ...rest } = i;
        return { invitation: rest };
      },
      acceptInvitation(req) {
        for (const org of orgs.values()) {
          const i = org.invitations.find(
            (x) =>
              x.secret === req.invitationSecret &&
              x.state === InvitationState.PENDING,
          );
          if (i === undefined) continue;
          if (org.members.some((m) => m.userId === userId))
            throw new ConnectError(
              "you are already a member",
              Code.AlreadyExists,
            );
          i.state = InvitationState.ACCEPTED;
          const m = member(
            org.organization.organizationId,
            userId,
            i.role,
            "Mina Kim",
          );
          m.joinTime = ts();
          org.members.push(m);
          audit(
            org,
            AuditAction.MEMBER_JOINED,
            AuditTargetKind.MEMBERSHIP,
            userId,
            userId,
          );
          return { organization: withRole(org, i.role), member: m };
        }
        throw new ConnectError(
          "invitation not found or no longer valid",
          Code.NotFound,
        );
      },
      listMembers(req) {
        const { org } = membership(req.organizationId);
        const p = page(org.members, req);
        return { members: p.items, nextPageToken: p.nextPageToken };
      },
      updateMemberRole(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const m = org.members.find((x) => x.userId === req.userId);
        if (m === undefined)
          throw new ConnectError("member not found", Code.NotFound);
        if (m.role === Role.OWNER || m.userId === userId)
          throw new ConnectError(
            "cannot change this role",
            Code.PermissionDenied,
          );
        if (req.role !== Role.ADMIN && req.role !== Role.MEMBER)
          throw new ConnectError(
            "role must be ADMIN or MEMBER",
            Code.InvalidArgument,
          );
        m.role = req.role;
        audit(
          org,
          AuditAction.MEMBER_ROLE_CHANGED,
          AuditTargetKind.MEMBERSHIP,
          m.userId,
          m.userId,
        );
        return { member: m };
      },
      removeMember(req) {
        const { org, role } = membership(req.organizationId);
        const m = org.members.find((x) => x.userId === req.userId);
        if (m === undefined)
          throw new ConnectError("member not found", Code.NotFound);
        if (m.role === Role.OWNER)
          throw new ConnectError(
            "the owner cannot leave or be removed",
            Code.FailedPrecondition,
          );
        if (m.userId !== userId) requireAdmin(role);
        org.members = org.members.filter((x) => x.userId !== req.userId);
        for (const k of org.apiKeys) {
          if (k.userId === req.userId && k.state === ApiKeyState.ACTIVE) {
            k.state = ApiKeyState.REVOKED;
            k.revokedAt = ts();
          }
        }
        audit(
          org,
          AuditAction.MEMBER_REMOVED,
          AuditTargetKind.MEMBERSHIP,
          m.userId,
          m.userId,
        );
        return {};
      },
    });

    router.service(OwnSandLinkService, {
      setOwnSandLink(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (req.ownsandOrganizationId === "" || req.ownsandApiKey === "")
          throw new ConnectError(
            "ownsand_organization_id and ownsand_api_key are required",
            Code.InvalidArgument,
          );
        if (!req.ownsandApiKey.startsWith("osk_"))
          throw new ConnectError(
            "OwnSand rejected the API key",
            Code.InvalidArgument,
          );
        if (
          org.link !== undefined &&
          org.link.ownsandOrganizationId !== req.ownsandOrganizationId
        )
          throw new ConnectError(
            "remove the link before you link another OwnSand organization",
            Code.FailedPrecondition,
          );
        for (const other of orgs.values()) {
          if (
            other !== org &&
            other.link?.ownsandOrganizationId === req.ownsandOrganizationId
          )
            throw new ConnectError(
              "another tide organization links this OwnSand organization",
              Code.AlreadyExists,
            );
        }
        org.link = create(OwnSandLinkSchema, {
          organizationId: req.organizationId,
          ownsandOrganizationId: req.ownsandOrganizationId,
          state: OwnSandLinkState.ACTIVE,
          credentialSet: true,
          credentialSetAt: ts(),
          createTime: org.link?.createTime ?? ts(),
        });
        audit(
          org,
          AuditAction.OWNSAND_LINK_SET,
          AuditTargetKind.OWNSAND_LINK,
          req.organizationId,
        );
        return { link: org.link };
      },
      getOwnSandLink(req) {
        const { org } = membership(req.organizationId);
        return { link: org.link };
      },
      removeOwnSandLink(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        for (const r of org.requests) {
          if (r.state === RequestState.QUEUED)
            endRequest(
              org,
              r,
              RequestState.CANCELED,
              RequestEndReason.INTEGRATION_REMOVED,
            );
        }
        for (const r of org.runners) {
          if (!TERMINAL_RUNNER_STATES.has(r.state))
            endRunner(org, r, RunnerEndReason.CLEAN_UP);
        }
        org.link = undefined;
        audit(
          org,
          AuditAction.OWNSAND_LINK_REMOVED,
          AuditTargetKind.OWNSAND_LINK,
          req.organizationId,
        );
        return {};
      },
    });

    router.service(GitHubService, {
      createGitHubInstallUrl(req) {
        const { role } = membership(req.organizationId);
        requireAdmin(role);
        const state = secret("state");
        const expiresAtMs = now() + 600_000;
        issuedStates.set(state, {
          organizationId: req.organizationId,
          expiresAtMs,
        });
        return {
          url: `https://github.com/apps/tide/installations/new?state=${encodeURIComponent(state)}`,
          expireTime: timestampFromMs(expiresAtMs),
        };
      },
      linkGitHubInstallation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (req.code === "" || req.state === "")
          throw new ConnectError(
            "code and state are required",
            Code.InvalidArgument,
          );
        // The development server and the e2e suite can use the fixed
        // "mock-state" fixture. Issued states are single-use.
        const issued = issuedStates.get(req.state);
        if (
          req.state !== "mock-state" &&
          (issued?.organizationId !== req.organizationId ||
            now() >= issued.expiresAtMs)
        )
          throw new ConnectError(
            "the installation state is not valid or has expired",
            Code.FailedPrecondition,
          );
        issuedStates.delete(req.state);
        if (
          org.installations.some(
            (i) => i.githubInstallationId === req.githubInstallationId,
          )
        )
          throw new ConnectError(
            "the installation is already linked",
            Code.AlreadyExists,
          );
        const installation = create(GitHubInstallationSchema, {
          installationId: uuidV7(),
          organizationId: req.organizationId,
          githubInstallationId: req.githubInstallationId,
          githubAccountLogin: `github-org-${req.githubInstallationId}`,
          githubAccountId: req.githubInstallationId + 1000n,
          state: GitHubInstallationState.ACTIVE,
          githubRunnerGroupId: 1n,
          runnerGroupName: "Default",
          createTime: ts(),
        });
        org.installations.push(installation);
        audit(
          org,
          AuditAction.GITHUB_INSTALLATION_LINKED,
          AuditTargetKind.GITHUB_INSTALLATION,
          installation.installationId,
        );
        return { installation };
      },
      listGitHubInstallations(req) {
        const { org } = membership(req.organizationId);
        const p = page(org.installations, req);
        return { installations: p.items, nextPageToken: p.nextPageToken };
      },
      listGitHubRunnerGroups(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        findInstallation(org, req.installationId);
        return {
          runnerGroups: [
            {
              githubRunnerGroupId: 1n,
              name: "Default",
              visibility: "all",
              isDefault: true,
            },
            {
              githubRunnerGroupId: 7n,
              name: "tide-private",
              visibility: "selected",
              isDefault: false,
            },
          ],
        };
      },
      updateGitHubInstallation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const i = findInstallation(org, req.installationId);
        const name =
          req.githubRunnerGroupId === 1n
            ? "Default"
            : req.githubRunnerGroupId === 7n
              ? "tide-private"
              : undefined;
        if (name === undefined)
          throw new ConnectError("runner group not found", Code.NotFound);
        i.githubRunnerGroupId = req.githubRunnerGroupId;
        i.runnerGroupName = name;
        audit(
          org,
          AuditAction.GITHUB_INSTALLATION_UPDATED,
          AuditTargetKind.GITHUB_INSTALLATION,
          i.installationId,
        );
        return { installation: i };
      },
      unlinkGitHubInstallation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const i = findInstallation(org, req.installationId);
        org.installations = org.installations.filter((x) => x !== i);
        for (const r of org.requests) {
          if (
            r.state === RequestState.QUEUED &&
            r.githubJob?.githubInstallationId === i.githubInstallationId
          )
            endRequest(
              org,
              r,
              RequestState.CANCELED,
              RequestEndReason.INTEGRATION_REMOVED,
            );
        }
        audit(
          org,
          AuditAction.GITHUB_INSTALLATION_UNLINKED,
          AuditTargetKind.GITHUB_INSTALLATION,
          i.installationId,
        );
        return {};
      },
    });

    router.service(AgentIntegrationService, {
      createAgentIntegration(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const name = req.name.trim();
        if (name === "" || name.length > 64)
          throw new ConnectError(
            "name must have 1 to 64 characters",
            Code.InvalidArgument,
          );
        if (org.integrations.some((i) => i.name === name))
          throw new ConnectError(
            "an integration with this name exists",
            Code.AlreadyExists,
          );
        if (req.vendor === AgentVendor.UNSPECIFIED)
          throw new ConnectError("vendor is required", Code.InvalidArgument);
        const id = uuidV7();
        const integration = create(AgentIntegrationSchema, {
          agentIntegrationId: id,
          organizationId: req.organizationId,
          vendor: req.vendor,
          name,
          settings: req.settings,
          credentials: credentialStates(req.vendor, [], req),
          webhookUrl:
            req.vendor === AgentVendor.OPENAI_CODEX
              ? `${MOCK_API_ORIGIN}/webhooks/openai/${id}`
              : "",
          createTime: ts(),
          updateTime: ts(),
        });
        org.integrations.push(integration);
        audit(
          org,
          AuditAction.AGENT_INTEGRATION_CREATED,
          AuditTargetKind.AGENT_INTEGRATION,
          id,
        );
        return { integration };
      },
      getAgentIntegration(req) {
        const { org } = membership(req.organizationId);
        return { integration: findIntegration(org, req.agentIntegrationId) };
      },
      listAgentIntegrations(req) {
        const { org } = membership(req.organizationId);
        const p = page(org.integrations, req);
        return { integrations: p.items, nextPageToken: p.nextPageToken };
      },
      updateAgentIntegration(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const i = findIntegration(org, req.agentIntegrationId);
        let name = i.name;
        if (hasPath(req.updateMask, "name")) {
          name = req.name.trim();
          if (name === "" || name.length > 64)
            throw new ConnectError(
              "name must have 1 to 64 characters",
              Code.InvalidArgument,
            );
        }
        const settings = hasPath(req.updateMask, "settings")
          ? req.settings
          : i.settings;
        const replaceCredentials = hasPath(req.updateMask, "credentials");
        const credentials = replaceCredentials
          ? credentialStates(i.vendor, i.credentials, req)
          : i.credentials;

        Object.assign(i, { name, settings, credentials });
        if (replaceCredentials) {
          audit(
            org,
            AuditAction.AGENT_INTEGRATION_CREDENTIALS_REPLACED,
            AuditTargetKind.AGENT_INTEGRATION,
            i.agentIntegrationId,
          );
        }
        i.updateTime = ts();
        audit(
          org,
          AuditAction.AGENT_INTEGRATION_UPDATED,
          AuditTargetKind.AGENT_INTEGRATION,
          i.agentIntegrationId,
        );
        return { integration: i };
      },
      deleteAgentIntegration(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const i = findIntegration(org, req.agentIntegrationId);
        if (
          org.profiles.some(
            (p) => p.agentIntegrationId === i.agentIntegrationId,
          )
        )
          throw new ConnectError(
            "a runner profile names this integration; delete the profile first",
            Code.FailedPrecondition,
          );
        org.integrations = org.integrations.filter((x) => x !== i);
        audit(
          org,
          AuditAction.AGENT_INTEGRATION_DELETED,
          AuditTargetKind.AGENT_INTEGRATION,
          i.agentIntegrationId,
        );
        return {};
      },
    });

    router.service(RunnerProfileService, {
      createRunnerProfile(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const input = req.profile;
        if (input === undefined)
          throw new ConnectError("profile is required", Code.InvalidArgument);
        validateProfile(input);
        if (org.profiles.some((p) => p.name === input.name))
          throw new ConnectError(
            "a runner profile with this name exists",
            Code.AlreadyExists,
          );
        if (input.workloadKind === WorkloadKind.AGENT_SESSION)
          findIntegration(org, input.agentIntegrationId);
        const p = profile(req.organizationId, {
          ...input,
          runnerProfileId: uuidV7(),
          priority:
            input.priority === Priority.UNSPECIFIED
              ? input.workloadKind === WorkloadKind.GITHUB_JOB
                ? Priority.HIGH
                : Priority.NORMAL
              : input.priority,
          createTime: ts(),
          updateTime: ts(),
        });
        org.profiles.push(p);
        audit(
          org,
          AuditAction.RUNNER_PROFILE_CREATED,
          AuditTargetKind.RUNNER_PROFILE,
          p.runnerProfileId,
        );
        return { profile: p };
      },
      getRunnerProfile(req) {
        const { org } = membership(req.organizationId);
        return { profile: findProfile(org, req.runnerProfileId) };
      },
      listRunnerProfiles(req) {
        const { org } = membership(req.organizationId);
        const items = org.profiles.filter(
          (p) =>
            req.workloadKind === WorkloadKind.UNSPECIFIED ||
            p.workloadKind === req.workloadKind,
        );
        const p = page(items, req);
        return { profiles: p.items, nextPageToken: p.nextPageToken };
      },
      updateRunnerProfile(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const p = findProfile(org, req.runnerProfileId);
        const input = req.profile;
        const paths = req.updateMask?.paths ?? [];
        if (input === undefined)
          throw new ConnectError("profile is required", Code.InvalidArgument);
        if (
          paths.includes("workload_kind") ||
          paths.includes("agent_integration_id")
        )
          throw new ConnectError(
            "workload_kind and agent_integration_id cannot change",
            Code.InvalidArgument,
          );
        const next = create(RunnerProfileSchema, { ...p });
        for (const path of paths) {
          switch (path) {
            case "name":
              next.name = input.name;
              break;
            case "workspace_path":
              next.workspacePath = input.workspacePath;
              break;
            case "guest_os":
              next.guestOs = input.guestOs;
              break;
            case "source":
              next.source = input.source;
              break;
            case "resources":
              next.resources = input.resources;
              break;
            case "network_mode":
              next.networkMode = input.networkMode;
              break;
            case "idle_ttl":
              next.idleTtl = input.idleTtl;
              break;
            case "max_ttl":
              next.maxTtl = input.maxTtl;
              break;
            case "priority":
              // Like the server: an update requires a concrete priority.
              if (input.priority === Priority.UNSPECIFIED)
                throw new ConnectError(
                  "invalid priority: must be HIGH, NORMAL or LOW",
                  Code.InvalidArgument,
                );
              next.priority = input.priority;
              break;
            case "max_queue_wait":
              next.maxQueueWait = input.maxQueueWait;
              break;
            case "warm_count":
              next.warmCount = input.warmCount;
              break;
            default:
              throw new ConnectError(
                `unknown mask path ${path}`,
                Code.InvalidArgument,
              );
          }
        }
        validateProfile(next);
        if (
          org.profiles.some(
            (other) =>
              other.runnerProfileId !== p.runnerProfileId &&
              other.name === next.name,
          )
        )
          throw new ConnectError(
            "a runner profile with this name exists",
            Code.AlreadyExists,
          );
        if (next.workloadKind === WorkloadKind.GITHUB_JOB)
          next.githubLabel = `tide-${next.name}`;
        next.updateTime = ts();
        org.profiles = org.profiles.map((x) => (x === p ? next : x));
        audit(
          org,
          AuditAction.RUNNER_PROFILE_UPDATED,
          AuditTargetKind.RUNNER_PROFILE,
          p.runnerProfileId,
        );
        return { profile: next };
      },
      deleteRunnerProfile(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const p = findProfile(org, req.runnerProfileId);
        org.profiles = org.profiles.filter((x) => x !== p);
        for (const r of org.requests) {
          if (
            r.runnerProfileId === p.runnerProfileId &&
            r.state === RequestState.QUEUED
          )
            endRequest(
              org,
              r,
              RequestState.CANCELED,
              RequestEndReason.PROFILE_DELETED,
            );
        }
        for (const r of org.runners) {
          if (
            r.runnerProfileId === p.runnerProfileId &&
            (r.state === RunnerState.WARM ||
              r.state === RunnerState.WAITING_FOR_WORK)
          )
            endRunner(org, r, RunnerEndReason.CLEAN_UP);
        }
        audit(
          org,
          AuditAction.RUNNER_PROFILE_DELETED,
          AuditTargetKind.RUNNER_PROFILE,
          p.runnerProfileId,
        );
        return {};
      },
    });

    router.service(WorkService, {
      listRequests(req) {
        const { org } = membership(req.organizationId);
        const active = org.requests.filter(
          (r) => !TERMINAL_REQUEST_STATES.has(r.state),
        );
        const ended = org.requests.filter((r) =>
          TERMINAL_REQUEST_STATES.has(r.state),
        );
        const items = [...active, ...(req.includeEnded ? ended : [])].filter(
          (r) =>
            (req.runnerProfileId === "" ||
              r.runnerProfileId === req.runnerProfileId) &&
            (req.kind === WorkloadKind.UNSPECIFIED || r.kind === req.kind) &&
            (req.states.length === 0 || req.states.includes(r.state)),
        );
        const p = page(items, req);
        return { requests: p.items, nextPageToken: p.nextPageToken };
      },
      getRequest(req) {
        const { org } = membership(req.organizationId);
        const r = org.requests.find((x) => x.requestId === req.requestId);
        if (r === undefined)
          throw new ConnectError("request not found", Code.NotFound);
        return { request: r };
      },
      cancelRequest(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const r = org.requests.find((x) => x.requestId === req.requestId);
        if (r === undefined)
          throw new ConnectError("request not found", Code.NotFound);
        if (r.state !== RequestState.QUEUED)
          throw new ConnectError(
            "only a queued request can be canceled",
            Code.FailedPrecondition,
          );
        endRequest(
          org,
          r,
          RequestState.CANCELED,
          RequestEndReason.CANCELED_BY_USER,
        );
        audit(
          org,
          AuditAction.REQUEST_CANCELED,
          AuditTargetKind.REQUEST,
          r.requestId,
        );
        return { request: r };
      },
      listRunners(req) {
        const { org } = membership(req.organizationId);
        const items = org.runners.filter(
          (r) =>
            (req.includeEnded || !TERMINAL_RUNNER_STATES.has(r.state)) &&
            (req.runnerProfileId === "" ||
              r.runnerProfileId === req.runnerProfileId) &&
            (req.states.length === 0 || req.states.includes(r.state)),
        );
        const p = page(items, req);
        return { runners: p.items, nextPageToken: p.nextPageToken };
      },
      getRunner(req) {
        const { org } = membership(req.organizationId);
        const r = org.runners.find((x) => x.runnerId === req.runnerId);
        if (r === undefined)
          throw new ConnectError("runner not found", Code.NotFound);
        return { runner: r };
      },
      deleteRunner(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const r = org.runners.find((x) => x.runnerId === req.runnerId);
        if (r === undefined)
          throw new ConnectError("runner not found", Code.NotFound);
        endRunner(org, r, RunnerEndReason.DELETED_BY_USER);
        audit(
          org,
          AuditAction.RUNNER_DELETED,
          AuditTargetKind.RUNNER,
          r.runnerId,
        );
        return { runner: r };
      },
      getCapacity(req) {
        const { org } = membership(req.organizationId);
        return { devices: org.link === undefined ? [] : org.devices };
      },
      async *watchQueue(req, ctx: HandlerContext) {
        const { org } = membership(req.organizationId);
        const queue: MessageInitShape<typeof WatchQueueResponseSchema>[] = [];
        let wake: (() => void) | undefined;
        const watcher: Watcher = (event) => {
          queue.push(event);
          wake?.();
        };
        const set = watchers.get(req.organizationId) ?? new Set<Watcher>();
        watchers.set(req.organizationId, set);
        set.add(watcher);
        try {
          for (const r of org.requests) {
            if (!TERMINAL_REQUEST_STATES.has(r.state))
              yield { event: { case: "request", value: r } };
          }
          for (const r of org.runners) {
            if (!TERMINAL_RUNNER_STATES.has(r.state))
              yield { event: { case: "runner", value: r } };
          }
          yield { event: { case: "snapshotComplete", value: {} } };
          const endAt =
            options.watchQueueEndsAfterMs === undefined
              ? undefined
              : now() + options.watchQueueEndsAfterMs;
          for (;;) {
            while (queue.length > 0) yield queue.shift()!;
            const remaining = endAt === undefined ? 10_000 : endAt - now();
            if (remaining <= 0) return;
            await new Promise<void>((resolve) => {
              const timer = setTimeout(resolve, Math.min(remaining, 10_000));
              wake = () => {
                clearTimeout(timer);
                resolve();
              };
              ctx.signal.addEventListener("abort", () => wake?.(), {
                once: true,
              });
            });
            wake = undefined;
            if (ctx.signal.aborted) return;
            if (queue.length === 0 && (endAt === undefined || now() < endAt)) {
              yield { event: { case: "keepalive", value: {} } };
            }
          }
        } finally {
          set.delete(watcher);
        }
      },
    });

    router.service(ApiKeyService, {
      createApiKey(req) {
        const { org } = membership(req.organizationId);
        const name = req.name.trim();
        if (name === "" || name.length > 64)
          throw new ConnectError(
            "name must have 1 to 64 characters",
            Code.InvalidArgument,
          );
        if (
          req.expiresAt !== undefined &&
          Number(req.expiresAt.seconds) * 1000 <= now()
        )
          throw new ConnectError(
            "expires_at must be in the future",
            Code.InvalidArgument,
          );
        const value = secret("tide");
        const key = Object.assign(
          create(ApiKeySchema, {
            apiKeyId: uuidV7(),
            organizationId: req.organizationId,
            name,
            displayPrefix: value.slice(0, 9),
            state: ApiKeyState.ACTIVE,
            createdAt: ts(),
            expiresAt: req.expiresAt,
          }),
          { userId },
        );
        org.apiKeys.push(key);
        audit(
          org,
          AuditAction.API_KEY_CREATED,
          AuditTargetKind.API_KEY,
          key.apiKeyId,
          userId,
        );
        const { userId: _u, ...apiKey } = key;
        return { apiKey, secret: value };
      },
      listApiKeys(req) {
        const { org } = membership(req.organizationId);
        const p = page(
          org.apiKeys.filter((k) => k.userId === userId),
          req,
        );
        return {
          apiKeys: p.items.map(({ userId: _u, ...k }) => k),
          nextPageToken: p.nextPageToken,
        };
      },
      revokeApiKey(req) {
        const { org } = membership(req.organizationId);
        const k = org.apiKeys.find(
          (x) => x.apiKeyId === req.apiKeyId && x.userId === userId,
        );
        if (k === undefined)
          throw new ConnectError("API key not found", Code.NotFound);
        k.state = ApiKeyState.REVOKED;
        k.revokedAt = ts();
        audit(
          org,
          AuditAction.API_KEY_REVOKED,
          AuditTargetKind.API_KEY,
          k.apiKeyId,
          userId,
        );
        const { userId: _u, ...apiKey } = k;
        return { apiKey };
      },
    });

    router.service(DevSessionService, {
      listDevSessionProfiles(req) {
        const { org } = membership(req.organizationId);
        const items = org.profiles.filter(
          (p) => p.workloadKind === WorkloadKind.DEV_SESSION,
        );
        const p = page(items, req);
        return {
          profiles: p.items.map((x) => ({
            runnerProfileId: x.runnerProfileId,
            name: x.name,
            guestOs: x.guestOs,
            resources: x.resources,
            workspacePath: x.workspacePath,
            idleTtl: x.idleTtl,
            maxTtl: x.maxTtl,
            maxQueueWait: x.maxQueueWait,
          })),
          nextPageToken: p.nextPageToken,
        };
      },
      createDevSession() {
        throw new ConnectError(
          "the mock API does not create dev sessions",
          Code.Unimplemented,
        );
      },
      getDevSession(req) {
        const { org } = membership(req.organizationId);
        const s = org.devSessions.find(
          (x) => x.devSessionId === req.devSessionId,
        );
        if (s === undefined)
          throw new ConnectError("dev session not found", Code.NotFound);
        return { devSession: s };
      },
      listDevSessions(req) {
        const { org } = membership(req.organizationId);
        const items = org.devSessions.filter(
          (s) =>
            (req.includeEnded || !TERMINAL_REQUEST_STATES.has(s.state)) &&
            (req.owner.case !== "ownedByCaller" ||
              !req.owner.value ||
              s.ownerUserId === userId) &&
            (req.owner.case !== "ownerUserId" ||
              s.ownerUserId === req.owner.value) &&
            (req.runnerProfileId === "" ||
              s.runnerProfileId === req.runnerProfileId) &&
            (req.states.length === 0 || req.states.includes(s.state)),
        );
        const p = page(items, req);
        return { devSessions: p.items, nextPageToken: p.nextPageToken };
      },
      deleteDevSession(req) {
        const { org, role } = membership(req.organizationId);
        const s = org.devSessions.find(
          (x) => x.devSessionId === req.devSessionId,
        );
        if (s === undefined)
          throw new ConnectError("dev session not found", Code.NotFound);
        if (s.ownerUserId !== userId) requireAdmin(role);
        if (TERMINAL_REQUEST_STATES.has(s.state))
          throw new ConnectError(
            "the dev session has ended",
            Code.FailedPrecondition,
          );
        const queued = s.state === RequestState.QUEUED;
        const r = org.requests.find((x) => x.requestId === s.devSessionId);
        const state = queued ? RequestState.CANCELED : RequestState.ENDED;
        const reason = queued
          ? RequestEndReason.CANCELED_BY_USER
          : RequestEndReason.SESSION_DELETED;
        if (r !== undefined) endRequest(org, r, state, reason);
        else {
          s.state = state;
          s.endReason = reason;
          s.endedAt = ts();
        }
        audit(
          org,
          AuditAction.DEV_SESSION_DELETED,
          AuditTargetKind.DEV_SESSION,
          s.devSessionId,
          s.ownerUserId,
        );
        return { devSession: s };
      },
      // The console never runs commands or transfers files.
      execDevSession() {
        throw new ConnectError(
          "not available in the mock API",
          Code.Unimplemented,
        );
      },
      attachDevSessionExec() {
        throw new ConnectError(
          "not available in the mock API",
          Code.Unimplemented,
        );
      },
      readDevSessionFile() {
        throw new ConnectError(
          "not available in the mock API",
          Code.Unimplemented,
        );
      },
      writeDevSessionFile() {
        throw new ConnectError(
          "not available in the mock API",
          Code.Unimplemented,
        );
      },
    });

    router.service(AuditService, {
      listAuditEvents(req) {
        const { org } = membership(req.organizationId);
        const p = page(org.audit, req);
        return { events: p.items, nextPageToken: p.nextPageToken };
      },
    });
  });
}

const VENDOR_CREDENTIAL_CASE: Record<AgentVendor, string | undefined> = {
  [AgentVendor.UNSPECIFIED]: undefined,
  [AgentVendor.CLAUDE_CODE]: "claudeCodeCredentials",
  [AgentVendor.CURSOR]: "cursorCredentials",
  [AgentVendor.OPENAI_CODEX]: "openaiCodexCredentials",
  [AgentVendor.DEVIN]: "devinCredentials",
};

/** The runner profile rules of the control plane contract. */
function validateProfile(p: RunnerProfile): void {
  const fail = (message: string) => {
    throw new ConnectError(message, Code.InvalidArgument);
  };
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(p.name) || p.name.length > 40)
    fail("name must be lowercase kebab-case with 1 to 40 characters");
  if (p.workloadKind === WorkloadKind.UNSPECIFIED)
    fail("workload_kind is required");
  if (
    p.workloadKind === WorkloadKind.AGENT_SESSION &&
    p.agentIntegrationId === ""
  )
    fail("agent_integration_id is required for AGENT_SESSION");
  if (p.workloadKind === WorkloadKind.DEV_SESSION && p.workspacePath === "")
    fail("workspace_path is required for DEV_SESSION");
  if (p.workloadKind !== WorkloadKind.DEV_SESSION && p.workspacePath !== "")
    fail("workspace_path is only for DEV_SESSION");
  if (p.guestOs === GuestOs.UNSPECIFIED) fail("guest_os is required");
  if (p.source.case === "baseImage" && p.source.value !== p.guestOs)
    fail("the base image must be of guest_os");
  if (p.source.case === undefined) fail("source is required");
  if (p.networkMode !== NetworkMode.OUTBOUND)
    fail("network_mode must be OUTBOUND");
  const r = p.resources;
  if (
    r === undefined ||
    r.vcpuCount <= 0 ||
    r.memoryMib <= 0n ||
    r.diskGib <= 0n
  )
    fail("resources must be greater than zero");
  const seconds = (d: Duration | undefined) =>
    d === undefined ? 0 : Number(d.seconds);
  if (seconds(p.idleTtl) <= 0) fail("idle_ttl must be greater than zero");
  if (seconds(p.maxTtl) <= 0 || seconds(p.maxTtl) > 7 * 86400)
    fail("max_ttl must be at most 7 days");
  if (seconds(p.maxQueueWait) <= 0 || seconds(p.maxQueueWait) > 86400)
    fail("max_queue_wait must be at most 24 hours");
  if (p.warmCount < 0 || p.warmCount > 10) fail("warm_count must be 0 to 10");
}
