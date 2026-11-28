// SPDX-License-Identifier: Apache-2.0
import { clone, create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import {
  Code,
  ConnectError,
  createRouterTransport,
  type HandlerContext,
  type Transport,
} from "@connectrpc/connect";
import {
  ConfigOptionSchema,
  type ConfigOption,
  type SessionMode,
  SessionModeSchema,
  AgentActivity,
  BridgeErrorKind,
  ConfigOptionCategory,
  PermissionOptionKind,
  PermissionOutcome,
  PlanEntryPriority,
  PlanEntryStatus,
  StopReason,
  ToolCallStatus,
  ToolKind,
} from "@delino/sandcrew-protos/v1/bridge_pb";
import {
  AgentVendor,
  CredentialScope,
  CredentialUpdateSource,
  GuestOs,
  PermissionMode,
  ProfileIneligibility,
  Role,
  SessionEndReason,
  SessionState,
  VendorCredentialKind,
} from "@delino/sandcrew-protos/v1/common_pb";
import {
  CredentialService,
  type SessionSecret,
  SessionSecretSchema,
  type VendorCredential,
  VendorCredentialSchema,
} from "@delino/sandcrew-protos/v1/credential_pb";
import {
  NotificationService,
  type PushSubscription,
  PushSubscriptionSchema,
} from "@delino/sandcrew-protos/v1/notification_pb";
import {
  type Organization,
  OrganizationSchema,
  OrganizationService,
  type OrganizationSettings,
  OrganizationSettingsSchema,
  type RunnerProfile,
  RunnerProfileSchema,
} from "@delino/sandcrew-protos/v1/organization_pb";
import {
  type AgentInfo,
  AgentInfoSchema,
  type Session,
  SessionSchema,
  SessionService,
  type TranscriptEvent,
  TranscriptEventSchema,
  type WatchSessionResponse,
  WatchSessionResponseSchema,
  type WatchSessionsResponse,
  WatchSessionsResponseSchema,
} from "@delino/sandcrew-protos/v1/session_pb";
import { uuidV7 } from "../api.ts";
import { MOCK_USER_ID } from "../auth.tsx";

/**
 * An in-browser fake of the SandCrew control plane for the development
 * server (`PUBLIC_SANDCREW_MOCK_API=true`), the unit tests and the
 * end-to-end suite. It applies the authorization rules of the control plane
 * contract, so hidden actions are also rejected. Production builds never
 * include it.
 */

export const MOCK_USER = MOCK_USER_ID;
/** Another member of the seeded organizations. */
export const MOCK_OTHER_USER = "mock-user-member";

/** Stable IDs of the seeded data, for tests. */
export const MOCK_IDS = {
  /** The caller is `owner`. Allowed profiles, credentials and sessions. */
  acme: "0194c000-0000-7000-8000-000000000001",
  /** The caller is `member`. No allowed runner profile. */
  northwind: "0194c000-0000-7000-8000-000000000002",
  /** The caller is `admin`. The SandRun session limit is reached. */
  globex: "0194c000-0000-7000-8000-000000000003",
  linuxProfile: "0194c100-0000-7000-8000-000000000001",
  macosProfile: "0194c100-0000-7000-8000-000000000002",
  windowsProfile: "0194c100-0000-7000-8000-000000000003",
  shortProfile: "0194c100-0000-7000-8000-000000000004",
  globexProfile: "0194c100-0000-7000-8000-000000000005",
  deletedProfile: "0194c100-0000-7000-8000-0000000000ff",
  /** Waits for a permission answer; its transcript has every event kind. */
  activeSession: "0194c200-0000-7000-8000-000000000001",
  /** Paused after a resume waited too long; limited retention. */
  pausedSession: "0194c200-0000-7000-8000-000000000002",
  /** Ended because the agent exited. */
  endedSession: "0194c200-0000-7000-8000-000000000003",
  /** Ended because the agent could not be installed. */
  installFailedSession: "0194c200-0000-7000-8000-000000000004",
  /** Another member's running session. */
  otherSession: "0194c200-0000-7000-8000-000000000005",
  /** The device of this session is offline. */
  disconnectedSession: "0194c200-0000-7000-8000-000000000006",
};

/** The VAPID public key of the mock (an uncompressed P-256 point). */
export const MOCK_VAPID_PUBLIC_KEY =
  "BEl62iUYgUivxIkv69yViEuiBIa-Ib9-SkvMeAtA3LFgDzkrxZJjSgSnfckjBJuBkr3qBUYIHBQFLXYp5Nksh8U";

/** The message of the SandRun session limit (control plane contract). */
export const SESSION_LIMIT_MESSAGE =
  "You have reached the session limit of this organization. Delete a session or ask an administrator.";

export interface MockTransportOptions {
  /** The delay of each simulated agent step. Default 400 ms. */
  agentStepMs?: number;
  /** Fails the first ListSessions call with `unavailable`. */
  failFirstSessionList?: boolean;
  /** Fails every ListOrganizations call with this code. */
  failOrganizations?: Code;
  /** Delays every unary response, for loading states. */
  responseDelayMs?: number;
}

interface OrgState {
  organization: Organization;
  settings: OrganizationSettings;
  profiles: RunnerProfile[];
  /** The SandRun per-user dev session limit, if any. */
  sessionLimit: number | undefined;
}

interface SessionRecord {
  session: Session;
  events: TranscriptEvent[];
}

type Listener<T> = (value: T) => void;

const MINUTE_MS = 60_000;
const DAY_MS = 24 * 3600_000;

const CLAUDE_MODES: SessionMode[] = [
  create(SessionModeSchema, {
    modeId: "default",
    name: "Default",
    description: "Ask before edits",
  }),
  create(SessionModeSchema, {
    modeId: "plan",
    name: "Plan",
    description: "Plan without changing files",
  }),
];

function claudeConfigOptions(current = "sonnet"): ConfigOption[] {
  return [
    create(ConfigOptionSchema, {
      configOptionId: "model",
      name: "Model",
      category: ConfigOptionCategory.MODEL,
      currentValueId: current,
      values: [
        { valueId: "sonnet", name: "Sonnet" },
        { valueId: "opus", name: "Opus" },
      ],
    }),
  ];
}

/** A changed copy of the agent information of a session. */
function withAgent(
  agent: AgentInfo | undefined,
  change: (agent: AgentInfo) => void,
): AgentInfo {
  const next =
    agent === undefined
      ? create(AgentInfoSchema)
      : clone(AgentInfoSchema, agent);
  change(next);
  return next;
}

type EventInit = MessageInitShape<typeof TranscriptEventSchema>["event"];

/** The fake control plane state and its RPC handlers. */
export function createMockTransport(
  options: MockTransportOptions = {},
): Transport {
  const stepMs = options.agentStepMs ?? 400;
  let failSessionList = options.failFirstSessionList ?? false;
  const now = () => Date.now();
  const orgs = new Map<string, OrgState>();
  const credentials: VendorCredential[] = [];
  const secrets: {
    organizationId: string;
    userId: string;
    secret: SessionSecret;
  }[] = [];
  const pushSubscriptions: PushSubscription[] = [];
  const sessions = new Map<string, SessionRecord>();
  const sessionListeners = new Set<
    Listener<{ organizationId: string; response: WatchSessionsResponse }>
  >();
  const transcriptListeners = new Map<
    string,
    Set<Listener<WatchSessionResponse>>
  >();

  // ---- Seed data ----------------------------------------------------------

  const profile = (
    id: string,
    name: string,
    guestOs: GuestOs,
    ineligibility: ProfileIneligibility[] = [],
  ) =>
    create(RunnerProfileSchema, {
      runnerProfileId: id,
      name,
      guestOs,
      vcpuCount: 4,
      memoryMib: 8192n,
      diskGib: 64n,
      selectable: ineligibility.length === 0,
      ineligibility,
    });

  const addOrg = (
    organizationId: string,
    name: string,
    role: Role,
    profiles: RunnerProfile[],
    allowed: string[],
    sessionLimit?: number,
  ) => {
    orgs.set(organizationId, {
      organization: create(OrganizationSchema, {
        organizationId,
        name,
        callerRole: role,
      }),
      settings: create(OrganizationSettingsSchema, {
        organizationId,
        allowedRunnerProfileIds: allowed,
        pauseDelay: durationFromMs(10 * MINUTE_MS),
        pausedSessionRetention: {
          retention: { case: "duration", value: durationFromMs(30 * DAY_MS) },
        },
        updateTime: timestampFromMs(now() - DAY_MS),
      }),
      profiles,
      sessionLimit,
    });
  };

  addOrg(
    MOCK_IDS.acme,
    "Acme Robotics",
    Role.OWNER,
    [
      profile(MOCK_IDS.linuxProfile, "Linux 4 vCPU", GuestOs.LINUX),
      profile(MOCK_IDS.macosProfile, "macOS 4 vCPU", GuestOs.MACOS),
      profile(MOCK_IDS.windowsProfile, "Windows 4 vCPU", GuestOs.WINDOWS, [
        ProfileIneligibility.GUEST_OS_UNSUPPORTED,
        ProfileIneligibility.NETWORK_NOT_OUTBOUND,
      ]),
      profile(MOCK_IDS.shortProfile, "Short-lived Linux", GuestOs.LINUX, [
        ProfileIneligibility.HAS_MAXIMUM_LIFETIME,
      ]),
    ],
    [MOCK_IDS.linuxProfile, MOCK_IDS.macosProfile, MOCK_IDS.deletedProfile],
  );
  addOrg(
    MOCK_IDS.northwind,
    "Northwind Labs",
    Role.MEMBER,
    [profile("0194c100-0000-7000-8000-000000000010", "Linux", GuestOs.LINUX)],
    [],
  );
  addOrg(
    MOCK_IDS.globex,
    "Globex",
    Role.ADMIN,
    [profile(MOCK_IDS.globexProfile, "Linux 2 vCPU", GuestOs.LINUX)],
    [MOCK_IDS.globexProfile],
    0,
  );

  const addCredential = (
    organizationId: string,
    scope: CredentialScope,
    vendor: AgentVendor,
    kind: VendorCredentialKind,
    ownerUserId = "",
  ) => {
    const credential = create(VendorCredentialSchema, {
      vendorCredentialId: uuidV7(),
      scope,
      vendor,
      kind,
      ownerUserId,
      createdAt: timestampFromMs(now() - 2 * DAY_MS),
      valueSetAt: timestampFromMs(now() - 2 * DAY_MS),
      updatedBy: CredentialUpdateSource.USER,
    });
    // Remember the organization through a side table.
    credentialOrg.set(credential.vendorCredentialId, organizationId);
    credentials.push(credential);
    return credential;
  };
  const credentialOrg = new Map<string, string>();
  addCredential(
    MOCK_IDS.acme,
    CredentialScope.ORGANIZATION,
    AgentVendor.CLAUDE_CODE,
    VendorCredentialKind.API_KEY,
  );
  addCredential(
    MOCK_IDS.globex,
    CredentialScope.ORGANIZATION,
    AgentVendor.CLAUDE_CODE,
    VendorCredentialKind.API_KEY,
  );
  addCredential(
    MOCK_IDS.northwind,
    CredentialScope.USER,
    AgentVendor.OPENAI_CODEX,
    VendorCredentialKind.CODEX_AUTH_JSON,
    MOCK_USER,
  );
  secrets.push({
    organizationId: MOCK_IDS.acme,
    userId: MOCK_USER,
    secret: create(SessionSecretSchema, {
      sessionSecretId: uuidV7(),
      name: "GITHUB_TOKEN",
      createdAt: timestampFromMs(now() - DAY_MS),
      valueSetAt: timestampFromMs(now() - DAY_MS),
    }),
  });

  // ---- Sessions and transcripts -------------------------------------------

  const publishSession = (record: SessionRecord) => {
    const { session } = record;
    for (const listener of sessionListeners) {
      listener({
        organizationId: session.organizationId,
        response: create(WatchSessionsResponseSchema, {
          event: { case: "session", value: session },
        }),
      });
    }
    for (const listener of transcriptListeners.get(session.sessionId) ?? []) {
      listener(
        create(WatchSessionResponseSchema, {
          event: { case: "session", value: session },
        }),
      );
    }
  };

  const setState = (
    record: SessionRecord,
    state: SessionState,
    changes: Partial<Omit<Session, "$typeName">> = {},
  ) => {
    record.session = { ...record.session, ...changes, state };
    publishSession(record);
  };

  const append = (record: SessionRecord, event: EventInit) => {
    const transcriptEvent = create(TranscriptEventSchema, {
      sequence: BigInt(record.events.length + 1),
      time: timestampFromMs(now()),
      event,
    });
    record.events.push(transcriptEvent);
    for (const listener of transcriptListeners.get(record.session.sessionId) ??
      []) {
      listener(
        create(WatchSessionResponseSchema, {
          event: { case: "transcriptEvent", value: transcriptEvent },
        }),
      );
    }
  };

  const text = (value: string) => ({
    content: { block: { case: "text" as const, value } },
  });

  const addSession = (
    init: MessageInitShape<typeof SessionSchema>,
    events: EventInit[] = [],
  ) => {
    const record: SessionRecord = {
      session: create(SessionSchema, {
        permissionMode: PermissionMode.AUTO_APPROVE,
        guestOs: GuestOs.LINUX,
        createdAt: timestampFromMs(now() - 3 * 3600_000),
        startedAt: timestampFromMs(now() - 3 * 3600_000 + MINUTE_MS),
        ...init,
      }),
      events: [],
    };
    for (const event of events) append(record, event);
    sessions.set(record.session.sessionId, record);
    return record;
  };

  const hello = (vendor: AgentVendor): EventInit => ({
    case: "hello",
    value: {
      bridgeVersion: "1.0.0",
      protocolVersion: { major: 1, minor: 0 },
      vendor,
      agentVersion: "2.1.0",
      adapterVersion: "0.9.0",
      guestOs: GuestOs.LINUX,
    },
  });

  addSession(
    {
      sessionId: MOCK_IDS.activeSession,
      organizationId: MOCK_IDS.acme,
      ownerUserId: MOCK_USER,
      label: "Fix the flaky upload test",
      vendor: AgentVendor.CLAUDE_CODE,
      runnerProfileId: MOCK_IDS.linuxProfile,
      state: SessionState.WAITING_FOR_INPUT,
      permissionMode: PermissionMode.ASK,
      agent: create(AgentInfoSchema, {
        agentVersion: "2.1.0",
        currentModeId: "default",
        availableModes: CLAUDE_MODES,
        configOptions: claudeConfigOptions(),
      }),
    },
    [
      hello(AgentVendor.CLAUDE_CODE),
      {
        case: "modeChanged",
        value: {
          currentModeId: "default",
          availableModes: CLAUDE_MODES,
          permissionMode: PermissionMode.AUTO_APPROVE,
        },
      },
      { case: "configOptions", value: { options: claudeConfigOptions() } },
      { case: "inputAccepted", value: { inputId: "input-1" } },
      {
        case: "userMessageChunk",
        value: text(
          "Clone https://github.com/example/uploader and fix the flaky upload test.",
        ),
      },
      { case: "sessionState", value: { state: AgentActivity.WORKING } },
      {
        case: "agentThoughtChunk",
        value: text("The test probably races with the retry timer."),
      },
      {
        case: "plan",
        value: {
          entries: [
            {
              content: "Clone the repository",
              priority: PlanEntryPriority.HIGH,
              status: PlanEntryStatus.COMPLETED,
            },
            {
              content: "Fix the race in the upload test",
              priority: PlanEntryPriority.HIGH,
              status: PlanEntryStatus.IN_PROGRESS,
            },
            {
              content: "Run the test suite",
              priority: PlanEntryPriority.MEDIUM,
              status: PlanEntryStatus.PENDING,
            },
          ],
        },
      },
      {
        case: "toolCall",
        value: {
          toolCallId: "tool-1",
          title: "git clone https://github.com/example/uploader",
          kind: ToolKind.EXECUTE,
          status: ToolCallStatus.IN_PROGRESS,
          rawInput:
            '{"command":"git clone https://github.com/example/uploader"}',
        },
      },
      {
        case: "permissionRequest",
        value: {
          permissionRequestId: "perm-1",
          toolCallId: "tool-1",
          options: [
            {
              optionId: "allow",
              name: "Allow",
              kind: PermissionOptionKind.ALLOW_ONCE,
            },
            {
              optionId: "reject",
              name: "Reject",
              kind: PermissionOptionKind.REJECT_ONCE,
            },
          ],
        },
      },
      {
        case: "permissionResponse",
        value: {
          permissionRequestId: "perm-1",
          outcome: PermissionOutcome.SELECTED,
          optionId: "allow",
          autoApproved: true,
        },
      },
      {
        case: "toolCallUpdate",
        value: {
          toolCallId: "tool-1",
          status: ToolCallStatus.COMPLETED,
          rawOutput: '{"exitCode":0,"stdout":"Cloning into uploader..."}',
        },
      },
      {
        case: "agentMessageChunk",
        value: text("I cloned the repository. The test waits on a **fixed** "),
      },
      {
        case: "agentMessageChunk",
        value: text(
          "timeout. See [the docs](https://example.com/docs) and `upload_test.go`.",
        ),
      },
      {
        case: "toolCall",
        value: {
          toolCallId: "tool-2",
          title: "Edit upload_test.go",
          kind: ToolKind.EDIT,
          status: ToolCallStatus.COMPLETED,
          content: [
            {
              content: {
                case: "diff",
                value: {
                  path: "uploader/upload_test.go",
                  oldText:
                    "func TestUpload(t *testing.T) {\n\ttime.Sleep(time.Second)\n}\n",
                  newText:
                    "func TestUpload(t *testing.T) {\n\twaitForRetry(t)\n}\n",
                },
              },
            },
          ],
          locations: [{ path: "uploader/upload_test.go", line: 12 }],
          rawInput: '{"path":"uploader/upload_test.go"}',
        },
      },
      {
        case: "unknownUpdate",
        value: {
          sessionUpdate: "usage_update",
          rawJson: '{"sessionUpdate":"usage_update","used":1200}',
        },
      },
      {
        case: "bridgeError",
        value: {
          kind: BridgeErrorKind.OPTION_REJECTED,
          message: "The agent rejected the option thought_level.",
        },
      },
      { case: "turnEnded", value: { stopReason: StopReason.END_TURN } },
      { case: "sessionState", value: { state: AgentActivity.IDLE } },
      {
        case: "modeChanged",
        value: {
          currentModeId: "default",
          availableModes: CLAUDE_MODES,
          permissionMode: PermissionMode.ASK,
        },
      },
      { case: "userMessageChunk", value: text("Run the tests now.") },
      { case: "sessionState", value: { state: AgentActivity.WORKING } },
      {
        case: "toolCall",
        value: {
          toolCallId: "tool-3",
          title: "go test ./...",
          kind: ToolKind.EXECUTE,
          status: ToolCallStatus.PENDING,
          rawInput: '{"command":"go test ./..."}',
        },
      },
      {
        case: "permissionRequest",
        value: {
          permissionRequestId: "perm-2",
          toolCallId: "tool-3",
          options: [
            {
              optionId: "allow",
              name: "Allow once",
              kind: PermissionOptionKind.ALLOW_ONCE,
            },
            {
              optionId: "always",
              name: "Always allow",
              kind: PermissionOptionKind.ALLOW_ALWAYS,
            },
            {
              optionId: "reject",
              name: "Reject",
              kind: PermissionOptionKind.REJECT_ONCE,
            },
          ],
        },
      },
      {
        case: "sessionState",
        value: { state: AgentActivity.WAITING_FOR_PERMISSION },
      },
    ],
  );

  addSession(
    {
      sessionId: MOCK_IDS.pausedSession,
      organizationId: MOCK_IDS.acme,
      ownerUserId: MOCK_USER,
      label: "Upgrade the build tools",
      vendor: AgentVendor.CLAUDE_CODE,
      runnerProfileId: MOCK_IDS.linuxProfile,
      state: SessionState.PAUSED,
      pausedAt: timestampFromMs(now() - 2 * 3600_000),
      scheduledDeletionTime: timestampFromMs(now() + 29 * DAY_MS),
      resumeWaitExpired: true,
    },
    [
      hello(AgentVendor.CLAUDE_CODE),
      { case: "userMessageChunk", value: text("Upgrade the build tools.") },
      { case: "agentMessageChunk", value: text("Done. Anything else?") },
      { case: "turnEnded", value: { stopReason: StopReason.END_TURN } },
      { case: "sessionState", value: { state: AgentActivity.IDLE } },
    ],
  );

  addSession(
    {
      sessionId: MOCK_IDS.endedSession,
      organizationId: MOCK_IDS.acme,
      ownerUserId: MOCK_USER,
      label: "Write release notes",
      vendor: AgentVendor.CLAUDE_CODE,
      runnerProfileId: MOCK_IDS.linuxProfile,
      state: SessionState.ENDED,
      endReason: SessionEndReason.AGENT_EXITED,
      endedAt: timestampFromMs(now() - DAY_MS),
    },
    [
      hello(AgentVendor.CLAUDE_CODE),
      { case: "userMessageChunk", value: text("Write the release notes.") },
      { case: "agentMessageChunk", value: text("The notes are in NOTES.md.") },
      { case: "turnEnded", value: { stopReason: StopReason.END_TURN } },
      {
        case: "agentExited",
        value: { result: { case: "exitCode", value: 1 } },
      },
    ],
  );

  addSession(
    {
      sessionId: MOCK_IDS.installFailedSession,
      organizationId: MOCK_IDS.acme,
      ownerUserId: MOCK_USER,
      label: "",
      vendor: AgentVendor.OPENAI_CODEX,
      runnerProfileId: MOCK_IDS.macosProfile,
      guestOs: GuestOs.MACOS,
      state: SessionState.ENDED,
      endReason: SessionEndReason.START_FAILED,
      endedAt: timestampFromMs(now() - 2 * DAY_MS),
    },
    [
      hello(AgentVendor.OPENAI_CODEX),
      {
        case: "bridgeError",
        value: {
          kind: BridgeErrorKind.AGENT_INSTALL_FAILED,
          message: "The agent download failed.",
        },
      },
    ],
  );

  addSession({
    sessionId: MOCK_IDS.otherSession,
    organizationId: MOCK_IDS.acme,
    ownerUserId: MOCK_OTHER_USER,
    label: "Refactor the billing module",
    vendor: AgentVendor.OPENAI_CODEX,
    runnerProfileId: MOCK_IDS.linuxProfile,
    state: SessionState.RUNNING,
  });

  addSession(
    {
      sessionId: MOCK_IDS.disconnectedSession,
      organizationId: MOCK_IDS.acme,
      ownerUserId: MOCK_USER,
      label: "Profile the image pipeline",
      vendor: AgentVendor.CLAUDE_CODE,
      runnerProfileId: MOCK_IDS.macosProfile,
      guestOs: GuestOs.MACOS,
      state: SessionState.DISCONNECTED,
    },
    [hello(AgentVendor.CLAUDE_CODE)],
  );

  // ---- Helpers ------------------------------------------------------------

  const delay = (ms: number) =>
    new Promise<void>((resolve) => setTimeout(resolve, ms));
  const respond = async () => {
    if (options.responseDelayMs) await delay(options.responseDelayMs);
  };

  const orgFor = (organizationId: string): OrgState => {
    const org = orgs.get(organizationId);
    if (org === undefined) {
      throw new ConnectError("organization not found", Code.NotFound);
    }
    return org;
  };
  const requireManager = (org: OrgState) => {
    const role = org.organization.callerRole;
    if (role !== Role.OWNER && role !== Role.ADMIN) {
      throw new ConnectError(
        "only owners and admins change these settings",
        Code.PermissionDenied,
      );
    }
  };
  const ownSession = (organizationId: string, sessionId: string) => {
    orgFor(organizationId);
    const record = sessions.get(sessionId);
    if (
      record === undefined ||
      record.session.organizationId !== organizationId ||
      record.session.ownerUserId !== MOCK_USER
    ) {
      throw new ConnectError("session not found", Code.NotFound);
    }
    return record;
  };
  /** Other members get the metadata fields only. */
  const visibleSession = (session: Session): Session =>
    session.ownerUserId === MOCK_USER
      ? session
      : create(SessionSchema, { ...session, agent: undefined });
  const profilesOf = (org: OrgState): RunnerProfile[] =>
    org.profiles.map((p) =>
      create(RunnerProfileSchema, {
        ...p,
        allowed: org.settings.allowedRunnerProfileIds.includes(
          p.runnerProfileId,
        ),
      }),
    );
  const requireText = (value: string, field: string) => {
    if (value.trim() === "") {
      throw new ConnectError(`${field} is empty`, Code.InvalidArgument);
    }
  };

  /** Runs the agent's answer to a prompt in steps. */
  const runTurn = async (record: SessionRecord, prompt: string) => {
    append(record, { case: "userMessageChunk", value: text(prompt) });
    append(record, {
      case: "sessionState",
      value: { state: AgentActivity.WORKING },
    });
    setState(record, SessionState.RUNNING);
    await delay(stepMs);
    if (record.session.state === SessionState.ENDED) return;
    if (/permission/i.test(prompt)) {
      const toolCallId = `tool-${uuidV7()}`;
      const permissionRequestId = `perm-${uuidV7()}`;
      append(record, {
        case: "toolCall",
        value: {
          toolCallId,
          title: "rm -rf build",
          kind: ToolKind.DELETE,
          status: ToolCallStatus.PENDING,
          rawInput: '{"command":"rm -rf build"}',
        },
      });
      append(record, {
        case: "permissionRequest",
        value: {
          permissionRequestId,
          toolCallId,
          options: [
            {
              optionId: "allow",
              name: "Allow once",
              kind: PermissionOptionKind.ALLOW_ONCE,
            },
            {
              optionId: "reject",
              name: "Reject",
              kind: PermissionOptionKind.REJECT_ONCE,
            },
          ],
        },
      });
      if (record.session.permissionMode === PermissionMode.AUTO_APPROVE) {
        await finishPermission(record, permissionRequestId, "allow", true);
      } else {
        append(record, {
          case: "sessionState",
          value: { state: AgentActivity.WAITING_FOR_PERMISSION },
        });
        setState(record, SessionState.WAITING_FOR_INPUT);
      }
      return;
    }
    append(record, {
      case: "agentMessageChunk",
      value: text(`I read your message: ${prompt}`),
    });
    await endTurn(record, StopReason.END_TURN);
  };

  const endTurn = async (record: SessionRecord, stopReason: StopReason) => {
    await delay(stepMs / 2);
    if (record.session.state === SessionState.ENDED) return;
    append(record, { case: "turnEnded", value: { stopReason } });
    append(record, {
      case: "sessionState",
      value: { state: AgentActivity.IDLE },
    });
    setState(record, SessionState.WAITING_FOR_INPUT);
  };

  const pendingPermission = (record: SessionRecord, id: string) => {
    let toolCallId: string | undefined;
    let answered = false;
    for (const e of record.events) {
      if (
        e.event.case === "permissionRequest" &&
        e.event.value.permissionRequestId === id
      ) {
        toolCallId = e.event.value.toolCallId;
      }
      if (
        e.event.case === "permissionResponse" &&
        e.event.value.permissionRequestId === id
      ) {
        answered = true;
      }
    }
    return answered ? undefined : toolCallId;
  };

  const finishPermission = async (
    record: SessionRecord,
    permissionRequestId: string,
    optionId: string,
    autoApproved: boolean,
  ) => {
    const toolCallId = pendingPermission(record, permissionRequestId);
    append(record, {
      case: "permissionResponse",
      value: {
        permissionRequestId,
        outcome: PermissionOutcome.SELECTED,
        optionId,
        autoApproved,
      },
    });
    append(record, {
      case: "sessionState",
      value: { state: AgentActivity.WORKING },
    });
    setState(record, SessionState.RUNNING);
    await delay(stepMs / 2);
    const allowed = optionId !== "reject";
    if (toolCallId !== undefined) {
      append(record, {
        case: "toolCallUpdate",
        value: {
          toolCallId,
          status: allowed ? ToolCallStatus.COMPLETED : ToolCallStatus.FAILED,
          rawOutput: allowed ? '{"exitCode":0}' : '{"rejected":true}',
        },
      });
    }
    append(record, {
      case: "agentMessageChunk",
      value: text(
        allowed ? "The command finished." : "I did not run the command.",
      ),
    });
    await endTurn(record, StopReason.END_TURN);
  };

  /** Resumes a paused session in steps, then runs `then`. */
  const resume = async (record: SessionRecord, then?: () => Promise<void>) => {
    setState(record, SessionState.RESUMING, {
      resumeWaitExpired: false,
      scheduledDeletionTime: undefined,
    });
    await delay(stepMs);
    setState(record, SessionState.WAITING_FOR_INPUT, { pausedAt: undefined });
    if (then) await then();
  };

  const isPaused = (record: SessionRecord) =>
    record.session.state === SessionState.PAUSED ||
    record.session.state === SessionState.PAUSING;

  const requireNotEnded = (record: SessionRecord) => {
    if (record.session.state === SessionState.ENDED) {
      throw new ConnectError("the session ended", Code.FailedPrecondition);
    }
  };

  const createSessionRecord = (
    organizationId: string,
    init: {
      vendor: AgentVendor;
      runnerProfileId: string;
      label: string;
      firstMessage: string;
      previousSessionId?: string;
    },
  ) => {
    const org = orgFor(organizationId);
    const profile = profilesOf(org).find(
      (p) => p.runnerProfileId === init.runnerProfileId,
    );
    if (profile === undefined || !profile.allowed || !profile.selectable) {
      throw new ConnectError(
        "the runner profile is not allowed for SandCrew sessions",
        Code.InvalidArgument,
      );
    }
    const hasCredential = credentials.some(
      (c) =>
        credentialOrg.get(c.vendorCredentialId) === organizationId &&
        c.vendor === init.vendor &&
        (c.scope === CredentialScope.ORGANIZATION ||
          c.ownerUserId === MOCK_USER),
    );
    if (!hasCredential) {
      throw new ConnectError(
        "Add a vendor credential for this agent in your settings.",
        Code.FailedPrecondition,
      );
    }
    const open = [...sessions.values()].filter(
      (r) =>
        r.session.organizationId === organizationId &&
        r.session.ownerUserId === MOCK_USER &&
        r.session.state !== SessionState.ENDED,
    ).length;
    if (org.sessionLimit !== undefined && open >= org.sessionLimit) {
      throw new ConnectError(SESSION_LIMIT_MESSAGE, Code.ResourceExhausted);
    }
    const record = addSession({
      sessionId: uuidV7(),
      organizationId,
      ownerUserId: MOCK_USER,
      label: init.label,
      vendor: init.vendor,
      runnerProfileId: init.runnerProfileId,
      guestOs: profile.guestOs,
      state: SessionState.QUEUED,
      permissionMode: PermissionMode.AUTO_APPROVE,
      createdAt: timestampFromMs(now()),
      startedAt: undefined,
      previousSessionId: init.previousSessionId ?? "",
    });
    publishSession(record);
    void (async () => {
      await delay(stepMs);
      setState(record, SessionState.STARTING, {
        startedAt: timestampFromMs(now()),
      });
      await delay(stepMs);
      append(record, hello(init.vendor));
      const modes = init.vendor === AgentVendor.CLAUDE_CODE ? CLAUDE_MODES : [];
      const configOptions =
        init.vendor === AgentVendor.CLAUDE_CODE ? claudeConfigOptions() : [];
      append(record, {
        case: "modeChanged",
        value: {
          currentModeId: modes[0]?.modeId ?? "",
          availableModes: modes,
          permissionMode: PermissionMode.AUTO_APPROVE,
        },
      });
      if (configOptions.length > 0) {
        append(record, {
          case: "configOptions",
          value: { options: configOptions },
        });
      }
      record.session = {
        ...record.session,
        agent: create(AgentInfoSchema, {
          agentVersion: "2.1.0",
          currentModeId: modes[0]?.modeId ?? "",
          availableModes: modes,
          configOptions,
        }),
      };
      await runTurn(record, init.firstMessage);
    })();
    return record;
  };

  /**
   * An async queue for a server stream: listeners push into it, and the
   * handler yields until the client aborts.
   */
  function streamQueue<T>(signal: AbortSignal) {
    const items: T[] = [];
    let wake: (() => void) | undefined;
    const push = (item: T) => {
      items.push(item);
      wake?.();
    };
    async function* drain(): AsyncGenerator<T> {
      while (!signal.aborted) {
        while (items.length > 0) yield items.shift()!;
        await new Promise<void>((resolve) => {
          wake = resolve;
          signal.addEventListener("abort", () => resolve(), { once: true });
        });
        wake = undefined;
      }
    }
    return { push, drain };
  }

  // ---- RPC handlers -------------------------------------------------------

  return createRouterTransport((router) => {
    router.service(OrganizationService, {
      async listOrganizations() {
        await respond();
        if (options.failOrganizations !== undefined) {
          throw new ConnectError(
            "organizations failed",
            options.failOrganizations,
          );
        }
        return {
          organizations: [...orgs.values()].map((o) => o.organization),
          nextPageToken: "",
        };
      },
      async getOrganizationSettings(req) {
        await respond();
        return { settings: orgFor(req.organizationId).settings };
      },
      async updateOrganizationSettings(req) {
        await respond();
        const org = orgFor(req.organizationId);
        requireManager(org);
        const paths = req.updateMask?.paths ?? [];
        const next = create(OrganizationSettingsSchema, org.settings);
        for (const path of paths) {
          switch (path) {
            case "allowed_runner_profile_ids": {
              const ids = req.settings?.allowedRunnerProfileIds ?? [];
              const profiles = profilesOf(org);
              for (const id of ids) {
                const p = profiles.find((x) => x.runnerProfileId === id);
                const keptMissing =
                  p === undefined &&
                  org.settings.allowedRunnerProfileIds.includes(id);
                if (!keptMissing && (p === undefined || !p.selectable)) {
                  throw new ConnectError(
                    `runner profile ${id} is not selectable`,
                    Code.InvalidArgument,
                  );
                }
              }
              next.allowedRunnerProfileIds = [...ids];
              break;
            }
            case "pause_delay": {
              const d = req.settings?.pauseDelay;
              const ms =
                d === undefined ? 10 * MINUTE_MS : Number(d.seconds) * 1000;
              if (
                ms < MINUTE_MS ||
                ms > 24 * 3600_000 ||
                (d?.nanos ?? 0) !== 0
              ) {
                throw new ConnectError(
                  "pause_delay must be 1 minute to 24 hours in whole seconds",
                  Code.InvalidArgument,
                );
              }
              next.pauseDelay = durationFromMs(ms);
              break;
            }
            case "paused_session_retention": {
              const r = req.settings?.pausedSessionRetention?.retention;
              if (r === undefined || r.case === undefined) {
                next.pausedSessionRetention = {
                  $typeName: "delino.sandcrew.v1.PausedSessionRetention",
                  retention: {
                    case: "duration",
                    value: durationFromMs(30 * DAY_MS),
                  },
                };
              } else if (r.case === "unlimited") {
                next.pausedSessionRetention = {
                  $typeName: "delino.sandcrew.v1.PausedSessionRetention",
                  retention: { case: "unlimited", value: true },
                };
              } else {
                const days = Number(r.value.seconds) / 86_400;
                if (!Number.isInteger(days) || days < 1 || days > 365) {
                  throw new ConnectError(
                    "paused_session_retention must be 1 to 365 whole days",
                    Code.InvalidArgument,
                  );
                }
                next.pausedSessionRetention = {
                  $typeName: "delino.sandcrew.v1.PausedSessionRetention",
                  retention: {
                    case: "duration",
                    value: durationFromMs(days * DAY_MS),
                  },
                };
              }
              break;
            }
            default:
              throw new ConnectError(
                `unknown path ${path}`,
                Code.InvalidArgument,
              );
          }
        }
        next.updateTime = timestampFromMs(now());
        org.settings = next;
        return { settings: next };
      },
      async listRunnerProfiles(req) {
        await respond();
        return {
          profiles: profilesOf(orgFor(req.organizationId)),
          nextPageToken: "",
        };
      },
    });

    router.service(CredentialService, {
      async setVendorCredential(req) {
        await respond();
        const org = orgFor(req.organizationId);
        if (req.scope === CredentialScope.ORGANIZATION) requireManager(org);
        if (req.value.byteLength === 0) {
          throw new ConnectError("value is empty", Code.InvalidArgument);
        }
        const ownerUserId = req.scope === CredentialScope.USER ? MOCK_USER : "";
        const index = credentials.findIndex(
          (c) =>
            credentialOrg.get(c.vendorCredentialId) === req.organizationId &&
            c.scope === req.scope &&
            c.vendor === req.vendor &&
            c.ownerUserId === ownerUserId,
        );
        if (index !== -1) credentials.splice(index, 1);
        const credential = addCredential(
          req.organizationId,
          req.scope,
          req.vendor,
          req.kind,
          ownerUserId,
        );
        credential.createdAt = timestampFromMs(now());
        credential.valueSetAt = timestampFromMs(now());
        return { credential };
      },
      async listVendorCredentials(req) {
        await respond();
        orgFor(req.organizationId);
        return {
          credentials: credentials.filter(
            (c) =>
              credentialOrg.get(c.vendorCredentialId) === req.organizationId &&
              (c.scope === CredentialScope.ORGANIZATION ||
                c.ownerUserId === MOCK_USER),
          ),
          nextPageToken: "",
        };
      },
      async deleteVendorCredential(req) {
        await respond();
        const org = orgFor(req.organizationId);
        const index = credentials.findIndex(
          (c) =>
            c.vendorCredentialId === req.vendorCredentialId &&
            credentialOrg.get(c.vendorCredentialId) === req.organizationId,
        );
        const credential = credentials[index];
        if (
          credential === undefined ||
          (credential.scope === CredentialScope.USER &&
            credential.ownerUserId !== MOCK_USER)
        ) {
          throw new ConnectError("credential not found", Code.NotFound);
        }
        if (credential.scope === CredentialScope.ORGANIZATION)
          requireManager(org);
        credentials.splice(index, 1);
        return {};
      },
      async setSessionSecret(req) {
        await respond();
        orgFor(req.organizationId);
        if (
          !/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(req.name) ||
          /^SANDCREW_/i.test(req.name)
        ) {
          throw new ConnectError(
            "the secret name is not valid",
            Code.InvalidArgument,
          );
        }
        const existing = secrets.find(
          (s) =>
            s.organizationId === req.organizationId &&
            s.userId === MOCK_USER &&
            s.secret.name === req.name,
        );
        if (existing) {
          existing.secret = create(SessionSecretSchema, {
            ...existing.secret,
            valueSetAt: timestampFromMs(now()),
          });
          return { secret: existing.secret };
        }
        const secret = create(SessionSecretSchema, {
          sessionSecretId: uuidV7(),
          name: req.name,
          createdAt: timestampFromMs(now()),
          valueSetAt: timestampFromMs(now()),
        });
        secrets.push({
          organizationId: req.organizationId,
          userId: MOCK_USER,
          secret,
        });
        return { secret };
      },
      async listSessionSecrets(req) {
        await respond();
        orgFor(req.organizationId);
        return {
          secrets: secrets
            .filter(
              (s) =>
                s.organizationId === req.organizationId &&
                s.userId === MOCK_USER,
            )
            .map((s) => s.secret),
          nextPageToken: "",
        };
      },
      async deleteSessionSecret(req) {
        await respond();
        orgFor(req.organizationId);
        const index = secrets.findIndex(
          (s) =>
            s.organizationId === req.organizationId &&
            s.userId === MOCK_USER &&
            s.secret.sessionSecretId === req.sessionSecretId,
        );
        if (index === -1)
          throw new ConnectError("secret not found", Code.NotFound);
        secrets.splice(index, 1);
        return {};
      },
    });

    router.service(SessionService, {
      async createSession(req) {
        await respond();
        requireText(req.firstMessage, "first_message");
        if ([...req.label].length > 100) {
          throw new ConnectError(
            "label is longer than 100 characters",
            Code.InvalidArgument,
          );
        }
        const record = createSessionRecord(req.organizationId, {
          vendor: req.vendor,
          runnerProfileId: req.runnerProfileId,
          label: req.label,
          firstMessage: req.firstMessage,
        });
        return { session: record.session };
      },
      async restartSessionFromTranscript(req) {
        await respond();
        requireText(req.firstMessage, "first_message");
        const previous = ownSession(req.organizationId, req.sessionId);
        if (previous.session.state !== SessionState.ENDED) {
          throw new ConnectError(
            "the session has not ended",
            Code.FailedPrecondition,
          );
        }
        const record = createSessionRecord(req.organizationId, {
          vendor: previous.session.vendor,
          runnerProfileId: previous.session.runnerProfileId,
          label: previous.session.label,
          firstMessage: req.firstMessage,
          previousSessionId: previous.session.sessionId,
        });
        return { session: record.session };
      },
      async getSession(req) {
        await respond();
        orgFor(req.organizationId);
        const record = sessions.get(req.sessionId);
        if (
          record === undefined ||
          record.session.organizationId !== req.organizationId
        ) {
          throw new ConnectError("session not found", Code.NotFound);
        }
        return { session: visibleSession(record.session) };
      },
      async listSessions(req) {
        await respond();
        orgFor(req.organizationId);
        if (failSessionList) {
          failSessionList = false;
          throw new ConnectError("sessions are unavailable", Code.Unavailable);
        }
        const list = [...sessions.values()]
          .map((r) => r.session)
          .filter((s) => s.organizationId === req.organizationId)
          .filter((s) =>
            req.owner.case === "ownedByCaller"
              ? s.ownerUserId === MOCK_USER
              : req.owner.case === "ownerUserId"
                ? s.ownerUserId === req.owner.value
                : true,
          )
          .filter(
            (s) => req.vendors.length === 0 || req.vendors.includes(s.vendor),
          )
          .filter(
            (s) => req.states.length === 0 || req.states.includes(s.state),
          )
          .sort((a, b) =>
            Number((b.createdAt?.seconds ?? 0n) - (a.createdAt?.seconds ?? 0n)),
          );
        const pageSize = Math.min(req.pageSize || 50, 200);
        const start = req.pageToken === "" ? 0 : Number(req.pageToken);
        const page = list.slice(start, start + pageSize);
        return {
          sessions: page.map(visibleSession),
          nextPageToken:
            start + pageSize < list.length ? String(start + pageSize) : "",
        };
      },
      async deleteSession(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        sessions.delete(req.sessionId);
        record.session = { ...record.session, state: SessionState.ENDED };
        for (const listener of sessionListeners) {
          listener({
            organizationId: req.organizationId,
            response: create(WatchSessionsResponseSchema, {
              event: {
                case: "sessionDeleted",
                value: { sessionId: req.sessionId },
              },
            }),
          });
        }
        return {};
      },
      async sendMessage(req) {
        await respond();
        requireText(req.text, "text");
        const record = ownSession(req.organizationId, req.sessionId);
        requireNotEnded(record);
        if (isPaused(record)) {
          void resume(record, () => runTurn(record, req.text));
        } else {
          void runTurn(record, req.text);
        }
        return { session: record.session };
      },
      async resumeSession(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        if (record.session.state !== SessionState.PAUSED) {
          throw new ConnectError(
            "the session is not paused",
            Code.FailedPrecondition,
          );
        }
        void resume(record);
        return { session: record.session };
      },
      async answerPermission(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        requireNotEnded(record);
        if (pendingPermission(record, req.permissionRequestId) === undefined) {
          throw new ConnectError(
            "the permission request is not pending",
            Code.FailedPrecondition,
          );
        }
        const answer = () =>
          finishPermission(
            record,
            req.permissionRequestId,
            req.optionId,
            false,
          );
        if (isPaused(record)) {
          void resume(record, answer);
        } else {
          void answer();
        }
        return { session: record.session };
      },
      async cancelTurn(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        requireNotEnded(record);
        if (isPaused(record)) {
          throw new ConnectError(
            "the session is paused",
            Code.FailedPrecondition,
          );
        }
        void endTurn(record, StopReason.CANCELLED);
        return { session: record.session };
      },
      async setSessionMode(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        requireNotEnded(record);
        if (isPaused(record)) {
          throw new ConnectError(
            "the session is paused",
            Code.FailedPrecondition,
          );
        }
        const agent = record.session.agent;
        const modes = agent?.availableModes ?? [];
        let modeId = agent?.currentModeId ?? "";
        let permissionMode = record.session.permissionMode;
        if (req.mode.case === "modeId") {
          if (!modes.some((m) => m.modeId === req.mode.value)) {
            throw new ConnectError("unknown mode", Code.InvalidArgument);
          }
          modeId = req.mode.value;
        } else if (req.mode.case === "permissionMode") {
          if (req.mode.value === PermissionMode.UNSPECIFIED) {
            throw new ConnectError(
              "permission_mode is unspecified",
              Code.InvalidArgument,
            );
          }
          permissionMode = req.mode.value;
        }
        append(record, {
          case: "modeChanged",
          value: {
            currentModeId: modeId,
            availableModes: modes,
            permissionMode,
          },
        });
        setState(record, record.session.state, {
          permissionMode,
          agent: withAgent(agent, (a) => {
            a.currentModeId = modeId;
          }),
        });
        return { session: record.session };
      },
      async setSessionConfigOption(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        requireNotEnded(record);
        if (isPaused(record)) {
          throw new ConnectError(
            "the session is paused",
            Code.FailedPrecondition,
          );
        }
        const current = record.session.agent?.configOptions ?? [];
        const option = current.find(
          (o) => o.configOptionId === req.configOptionId,
        );
        if (
          option === undefined ||
          !option.values.some((v) => v.valueId === req.valueId)
        ) {
          throw new ConnectError(
            "unknown option or value",
            Code.InvalidArgument,
          );
        }
        const options = current.map((o) =>
          o.configOptionId === req.configOptionId
            ? create(ConfigOptionSchema, { ...o, currentValueId: req.valueId })
            : o,
        );
        append(record, { case: "configOptions", value: { options } });
        setState(record, record.session.state, {
          agent: withAgent(record.session.agent, (a) => {
            a.configOptions = options;
          }),
        });
        return { session: record.session };
      },
      async getAgentOptions(req) {
        await respond();
        orgFor(req.organizationId);
        return req.vendor === AgentVendor.CLAUDE_CODE
          ? {
              availableModes: CLAUDE_MODES,
              configOptions: claudeConfigOptions(),
            }
          : { availableModes: [], configOptions: [] };
      },
      async listSessionEvents(req) {
        await respond();
        const record = ownSession(req.organizationId, req.sessionId);
        const pageSize = Math.min(req.pageSize || 200, 500);
        const after = record.events.filter(
          (e) => e.sequence > req.afterSequence,
        );
        const page = after.slice(0, pageSize);
        return {
          events: page,
          nextSequence: after.length > pageSize ? page.at(-1)!.sequence : 0n,
        };
      },
      async *watchSession(req, ctx: HandlerContext) {
        const record = ownSession(req.organizationId, req.sessionId);
        const queue = streamQueue<WatchSessionResponse>(ctx.signal);
        const listeners = transcriptListeners.get(req.sessionId) ?? new Set();
        transcriptListeners.set(req.sessionId, listeners);
        listeners.add(queue.push);
        try {
          for (const e of record.events) {
            if (e.sequence > req.afterSequence) {
              yield create(WatchSessionResponseSchema, {
                event: { case: "transcriptEvent", value: e },
              });
            }
          }
          yield create(WatchSessionResponseSchema, {
            event: { case: "caughtUp", value: {} },
          });
          yield* queue.drain();
        } finally {
          listeners.delete(queue.push);
        }
      },
      async *watchSessions(req, ctx: HandlerContext) {
        orgFor(req.organizationId);
        const queue = streamQueue<WatchSessionsResponse>(ctx.signal);
        const listener: Listener<{
          organizationId: string;
          response: WatchSessionsResponse;
        }> = ({ organizationId, response }) => {
          if (organizationId !== req.organizationId) return;
          queue.push(
            response.event.case === "session"
              ? create(WatchSessionsResponseSchema, {
                  event: {
                    case: "session",
                    value: visibleSession(response.event.value),
                  },
                })
              : response,
          );
        };
        sessionListeners.add(listener);
        try {
          yield create(WatchSessionsResponseSchema, {
            event: { case: "keepalive", value: {} },
          });
          yield* queue.drain();
        } finally {
          sessionListeners.delete(listener);
        }
      },
    });

    router.service(NotificationService, {
      async getWebPushConfig() {
        await respond();
        return { vapidPublicKey: MOCK_VAPID_PUBLIC_KEY };
      },
      async registerPushSubscription(req) {
        await respond();
        if (req.endpoint === "" || req.p256dhKey === "" || req.authKey === "") {
          throw new ConnectError(
            "the subscription is incomplete",
            Code.InvalidArgument,
          );
        }
        const subscription = create(PushSubscriptionSchema, {
          pushSubscriptionId: uuidV7(),
          label: req.label,
          endpointOrigin: new URL(req.endpoint).origin,
          createdAt: timestampFromMs(now()),
        });
        pushSubscriptions.push(subscription);
        return { subscription };
      },
      async listPushSubscriptions() {
        await respond();
        return { subscriptions: [...pushSubscriptions], nextPageToken: "" };
      },
      async deletePushSubscription(req) {
        await respond();
        const index = pushSubscriptions.findIndex(
          (s) => s.pushSubscriptionId === req.pushSubscriptionId,
        );
        if (index === -1)
          throw new ConnectError("subscription not found", Code.NotFound);
        pushSubscriptions.splice(index, 1);
        return {};
      },
    });
  });
}
