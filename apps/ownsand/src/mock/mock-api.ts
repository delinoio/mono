// SPDX-License-Identifier: Apache-2.0
import {
  clone,
  create,
  type MessageInitShape,
  toBinary,
} from "@bufbuild/protobuf";
import {
  type Duration,
  type Timestamp,
  TimestampSchema,
  timestampFromMs,
  timestampMs,
} from "@bufbuild/protobuf/wkt";
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import {
  type ApiKey,
  ApiKeySchema,
  ApiKeyService,
} from "@delino/ownsand-protos/v1/api_key_pb";
import {
  BillingAccountRole,
  BillingMeter,
  type BillingMeterUsage,
  BillingMeterUsageSchema,
  BillingPhase,
  BillingService,
  LinkableBillingAccountSchema,
  type OrganizationBilling,
  OrganizationBillingSchema,
} from "@delino/ownsand-protos/v1/billing_pb";
import {
  AgentUpdateState,
  AgentVersionPolicyMode,
  ApiKeyState,
  Arch,
  BaseImage,
  BillingReason,
  type DeviceLimits,
  DeviceLimitsSchema,
  DeviceState,
  DeviceStateSchema,
  EnrollmentTokenState,
  ErrorCode,
  ErrorDetailSchema,
  GuestOs,
  HostOs,
  InvitationState,
  NetworkMode,
  PrincipalKind,
  Role,
  SandboxEndReason,
  SandboxRestorePath,
  SandboxState,
  SnapshotExportState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";
import {
  type AgentVersionPolicy,
  AgentVersionPolicySchema,
  type Device,
  DeviceSchema,
  DeviceService,
  type EnrollmentToken,
  EnrollmentTokenSchema,
} from "@delino/ownsand-protos/v1/device_pb";
import {
  type Invitation,
  InvitationSchema,
  type Member,
  MemberSchema,
  MemberService,
} from "@delino/ownsand-protos/v1/member_pb";
import {
  type Organization,
  OrganizationSchema,
  OrganizationService,
} from "@delino/ownsand-protos/v1/organization_pb";
import {
  type CreateSandboxRequest,
  type Sandbox,
  CreateSandboxRequestSchema,
  SandboxSchema,
  type SandboxSource,
  SandboxService,
  SandboxSourceSchema,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import {
  type Snapshot,
  SnapshotSchema,
  SnapshotService,
  type Template,
  TemplateSchema,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import { ACCOUNTS_URL } from "../config.ts";
import { effectiveVcpuLimit, MAX_INT32 } from "../format.ts";
import { MAX_TTL_SECONDS } from "../forms.ts";
import { uuidV7 } from "../uuid.ts";

/**
 * An in-browser fake of the OwnSand control plane for the development
 * server (`PUBLIC_OWNSAND_MOCK_API=true`), the unit tests and the end-to-end
 * suite. It applies the role matrix, so hidden actions are also rejected.
 * Production builds never include it.
 */

export const MOCK_USER = "mock-user-owner";
/** A seeded invitation secret that joins the Contoso organization. */
export const MOCK_INVITATION_SECRET = "osi_mock_contoso_invitation";

export const MOCK_IDS = {
  acme: "0193a000-0000-7000-8000-000000000001",
  northwind: "0193a000-0000-7000-8000-000000000002",
  contoso: "0193a000-0000-7000-8000-000000000003",
  buildBox: "0193a000-0000-7000-8000-0000000000d1",
  studioMac: "0193a000-0000-7000-8000-0000000000d2",
  runningSandbox: "0193a000-0000-7000-8000-0000000000a1",
  pausedSandbox: "0193a000-0000-7000-8000-0000000000a2",
  deletedSandbox: "0193a000-0000-7000-8000-0000000000a3",
  unreachableSandbox: "0193a000-0000-7000-8000-0000000000a4",
  snapshot: "0193a000-0000-7000-8000-0000000000b1",
  template: "0193a000-0000-7000-8000-0000000000c1",
  acmeBillingAccount: "0193a000-0000-7000-8000-0000000000e1",
  labBillingAccount: "0193a000-0000-7000-8000-0000000000e2",
} as const;

/** Billing states that the mock API can seed for an organization. */
export enum MockBillingState {
  /** Linked to a billing account in good standing. */
  AllowedLinked = "allowed_linked",
  /** No billing account linked, blocked since a day ago. */
  BlockedUnlinked = "blocked_unlinked",
  /** Linked to a billing account with debt, blocked since a day ago. */
  BlockedLinked = "blocked_linked",
  /** Linked to a billing account with debt, blocked since four days ago. */
  Suspended = "suspended",
}

/** A delino-accounts billing account as the mock API knows it. */
export interface MockBillingAccount {
  billingAccountId: string;
  name: string;
  /** The mock user's role in the billing account. */
  role: BillingAccountRole;
  balanceUsdMicros: bigint;
  subscriptionActive: boolean;
}

/** The billing accounts of the mock user. */
export const MOCK_BILLING_ACCOUNTS: readonly MockBillingAccount[] = [
  {
    billingAccountId: MOCK_IDS.acmeBillingAccount,
    name: "Acme Robotics billing",
    role: BillingAccountRole.OWNER,
    balanceUsdMicros: 42_517_300n,
    subscriptionActive: true,
  },
  {
    billingAccountId: MOCK_IDS.labBillingAccount,
    name: "Research lab credits",
    role: BillingAccountRole.ADMIN,
    // Debt: a balance below zero.
    balanceUsdMicros: -1_234_567n,
    subscriptionActive: true,
  },
];

/**
 * Usage of a linked seeded organization this month, for every meter in
 * BillingMeter order, with amounts from the control plane formulas. The
 * RPC costs are below $0.01.
 */
const SEEDED_USAGE: readonly (readonly [BillingMeter, bigint, bigint])[] = [
  // 12.5 GiB of relay egress at $0.09 per GiB.
  [BillingMeter.RELAY_EGRESS, 13_421_772_800n, 1_125_000n],
  // 3 GiB stored for 10 days at $0.03 per GiB-month (730 hours).
  [BillingMeter.R2_STORAGE, 3n * 2n ** 30n * 864_000n, 29_590n],
  // 1,240 Class A calls at $4.50 per million.
  [BillingMeter.CLASS_A_RPC, 1_240n, 5_580n],
  // 18,400 Class B calls at $0.36 per million.
  [BillingMeter.CLASS_B_RPC, 18_400n, 6_624n],
];

const METERS = [
  BillingMeter.RELAY_EGRESS,
  BillingMeter.R2_STORAGE,
  BillingMeter.CLASS_A_RPC,
  BillingMeter.CLASS_B_RPC,
] as const;

/** The default billing state of each seeded organization. */
const DEFAULT_BILLING: Readonly<Record<string, MockBillingState>> = {
  [MOCK_IDS.acme]: MockBillingState.AllowedLinked,
  [MOCK_IDS.northwind]: MockBillingState.Suspended,
  [MOCK_IDS.contoso]: MockBillingState.BlockedUnlinked,
};

const HOUR = 3600;
const NANOS_PER_SECOND = 1_000_000_000n;
const MAX_TTL_NANOS = BigInt(MAX_TTL_SECONDS) * NANOS_PER_SECOND;
const DAY_MS = 24 * HOUR * 1000;
/** A non-paying organization becomes suspended 3 days after its block. */
const SUSPEND_AFTER_MS = 3 * DAY_MS;
const now = () => Date.now();
const ts = (offsetSeconds = 0): Timestamp =>
  timestampFromMs(now() + offsetSeconds * 1000);
const dur = (seconds: number): Duration =>
  ({ seconds: BigInt(seconds), nanos: 0 }) as Duration;

function ttlNanos(
  ttl: Duration | undefined,
  field: "idle_ttl" | "max_ttl",
): bigint {
  if (ttl === undefined) {
    throw new ConnectError(`${field} is required`, Code.InvalidArgument);
  }
  const { seconds, nanos } = ttl;
  if (
    seconds < 0n ||
    !Number.isInteger(nanos) ||
    nanos < 0 ||
    nanos >= Number(NANOS_PER_SECOND) ||
    (seconds === 0n && nanos === 0)
  ) {
    throw new ConnectError(
      `${field} must be a valid duration greater than zero`,
      Code.InvalidArgument,
    );
  }
  return seconds * NANOS_PER_SECOND + BigInt(nanos);
}

function addTtl(timestamp: Timestamp, durationNanos: bigint): Timestamp {
  const nanos = BigInt(timestamp.nanos) + durationNanos;
  return create(TimestampSchema, {
    seconds: timestamp.seconds + nanos / NANOS_PER_SECOND,
    nanos: Number(nanos % NANOS_PER_SECOND),
  });
}

function isPendingInvitation(
  { state, expireTime }: Invitation,
  cutoff: number,
): boolean {
  return (
    state === InvitationState.PENDING &&
    expireTime !== undefined &&
    timestampMs(expireTime) > cutoff
  );
}

interface OrgState {
  organization: Organization;
  members: Member[];
  invitations: { invitation: Invitation; secret: string }[];
  devices: Device[];
  tokens: EnrollmentToken[];
  apiKeys: ApiKey[];
  sandboxes: Sandbox[];
  /**
   * Create idempotency bookkeeping, kept apart from the user-visible sandbox
   * labels. Each key reserves the sandbox and encoded request for 24 hours.
   */
  createKeys: Map<
    string,
    { sandboxId: string; request: string; expiresAtMs: number }
  >;
  snapshots: Snapshot[];
  templates: Template[];
  policy: AgentVersionPolicy;
  billing: {
    /** The linked billing account, if any. */
    billingAccountId: string | undefined;
    /** Start of the non-payment in ms, if the organization is in one. */
    blockedSinceMs: number | undefined;
    /** Usage this month, one entry per meter in BillingMeter order. */
    usage: BillingMeterUsage[];
  };
}

function zeroUsage(): BillingMeterUsage[] {
  return METERS.map((meter) =>
    create(BillingMeterUsageSchema, {
      meter,
      quantity: 0n,
      amountUsdMicros: 0n,
    }),
  );
}

function seededUsage(): BillingMeterUsage[] {
  return SEEDED_USAGE.map(([meter, quantity, amountUsdMicros]) =>
    create(BillingMeterUsageSchema, { meter, quantity, amountUsdMicros }),
  );
}

/** Applies a seeded billing state to an organization. */
function applyBillingState(org: OrgState, state: MockBillingState): void {
  switch (state) {
    case MockBillingState.AllowedLinked:
      org.billing = {
        billingAccountId: MOCK_IDS.acmeBillingAccount,
        blockedSinceMs: undefined,
        usage: seededUsage(),
      };
      return;
    case MockBillingState.BlockedUnlinked:
      org.billing = {
        billingAccountId: undefined,
        blockedSinceMs: now() - DAY_MS,
        usage: zeroUsage(),
      };
      return;
    case MockBillingState.BlockedLinked:
      org.billing = {
        billingAccountId: MOCK_IDS.labBillingAccount,
        blockedSinceMs: now() - DAY_MS,
        usage: seededUsage(),
      };
      return;
    case MockBillingState.Suspended:
      org.billing = {
        billingAccountId: MOCK_IDS.labBillingAccount,
        blockedSinceMs: now() - 4 * DAY_MS,
        usage: seededUsage(),
      };
      // Like the suspension worker: running sandboxes are paused.
      for (const s of org.sandboxes) {
        if (s.state === SandboxState.RUNNING) s.state = SandboxState.PAUSED;
      }
      return;
  }
}

function member(
  orgId: string,
  userId: string,
  role: Role,
  displayName: string,
): Member {
  return create(MemberSchema, {
    organizationId: orgId,
    userId,
    role,
    displayName,
    joinTime: ts(-30 * 24 * HOUR),
  });
}

function emptyOrg(id: string, name: string): OrgState {
  return {
    organization: create(OrganizationSchema, {
      organizationId: id,
      name,
      createTime: ts(-60 * 24 * HOUR),
    }),
    members: [],
    invitations: [],
    devices: [],
    tokens: [],
    apiKeys: [],
    sandboxes: [],
    createKeys: new Map(),
    snapshots: [],
    templates: [],
    policy: create(AgentVersionPolicySchema, {
      organizationId: id,
      mode: AgentVersionPolicyMode.FOLLOW_DEFAULT,
    }),
    // A new organization has no link, so it is blocked from its creation.
    billing: {
      billingAccountId: undefined,
      blockedSinceMs: now(),
      usage: zeroUsage(),
    },
  };
}

function seed(): Map<string, OrgState> {
  const orgs = new Map<string, OrgState>();
  const acme = emptyOrg(MOCK_IDS.acme, "Acme Robotics");
  acme.members.push(
    member(MOCK_IDS.acme, MOCK_USER, Role.OWNER, "Mina Kim"),
    member(MOCK_IDS.acme, "user-admin-jun", Role.ADMIN, "Jun Park"),
    member(MOCK_IDS.acme, "user-member-ada", Role.MEMBER, ""),
  );
  acme.invitations.push({
    invitation: create(InvitationSchema, {
      invitationId: newMockId(),
      organizationId: MOCK_IDS.acme,
      role: Role.MEMBER,
      state: InvitationState.PENDING,
      createTime: ts(-HOUR),
      expireTime: ts(6 * 24 * HOUR),
      secretPrefix: "osi_7f3a",
    }),
    secret: "osi_7f3a_unused",
  });
  acme.devices.push(
    create(DeviceSchema, {
      deviceId: MOCK_IDS.buildBox,
      organizationId: MOCK_IDS.acme,
      name: "build-box-1",
      state: DeviceState.ONLINE,
      hostOs: HostOs.LINUX,
      arch: Arch.AMD64,
      agentVersion: "1.4.2",
      updateState: AgentUpdateState.UP_TO_DATE,
      capabilities: {
        logicalCpuCount: 32,
        memoryMib: 131072n,
        freeDiskBytes: 900n * 1024n ** 3n,
        guestOses: [GuestOs.LINUX],
        builtBaseImages: [BaseImage.LINUX],
      },
      limits: {
        vcpuCount: 16,
        memoryMib: 65536n,
        diskGib: 500n,
        vcpuOvercommitPercent: 150,
      },
      effectiveVcpuLimit: 24,
      effectiveVcpuLimitCount: 24n,
      allocation: {
        vcpuCount: 10,
        vcpuTotalCount: 10n,
        memoryMib: 20480n,
        diskGib: 80n,
      },
      createTime: ts(-50 * 24 * HOUR),
      lastSeenTime: ts(-5),
    }),
    create(DeviceSchema, {
      deviceId: MOCK_IDS.studioMac,
      organizationId: MOCK_IDS.acme,
      name: "studio-mac",
      state: DeviceState.OFFLINE,
      hostOs: HostOs.MACOS,
      arch: Arch.ARM64,
      agentVersion: "1.4.1",
      updateState: AgentUpdateState.PENDING_IDLE,
      capabilities: {
        logicalCpuCount: 12,
        memoryMib: 65536n,
        guestOses: [GuestOs.LINUX, GuestOs.MACOS],
        builtBaseImages: [BaseImage.LINUX, BaseImage.MACOS],
        macosGuestsSupported: true,
      },
      limits: {
        vcpuCount: 0,
        memoryMib: 0n,
        diskGib: 0n,
        vcpuOvercommitPercent: 100,
      },
      effectiveVcpuLimit: 0,
      effectiveVcpuLimitCount: 0n,
      allocation: {
        vcpuCount: 2,
        vcpuTotalCount: 2n,
        memoryMib: 4096n,
        diskGib: 20n,
      },
      createTime: ts(-3 * 24 * HOUR),
      lastSeenTime: ts(-2 * HOUR),
    }),
  );
  const sandbox = (
    id: string,
    deviceId: string,
    state: SandboxState,
    extra: Omit<MessageInitShape<typeof SandboxSchema>, "$typeName"> = {},
  ) =>
    create(SandboxSchema, {
      sandboxId: id,
      organizationId: MOCK_IDS.acme,
      deviceId,
      state,
      source: { source: { case: "baseImage", value: BaseImage.LINUX } },
      resources: { vcpuCount: 2, memoryMib: 4096n, diskGib: 20n },
      guestOs: GuestOs.LINUX,
      hostOs: HostOs.LINUX,
      arch: Arch.AMD64,
      network: { mode: NetworkMode.OUTBOUND },
      idleTtl: dur(HOUR),
      maxTtl: dur(24 * HOUR),
      idleDeadline: ts(HOUR),
      maxDeadline: ts(20 * HOUR),
      restorePath: SandboxRestorePath.BASE_IMAGE,
      createdBy: { kind: PrincipalKind.USER, id: MOCK_USER },
      createTime: ts(-4 * HOUR),
      ...extra,
    });
  acme.sandboxes.push(
    sandbox(MOCK_IDS.runningSandbox, MOCK_IDS.buildBox, SandboxState.RUNNING, {
      labels: { team: "ci", purpose: "integration-tests" },
    }),
    sandbox(MOCK_IDS.pausedSandbox, MOCK_IDS.buildBox, SandboxState.PAUSED, {
      labels: { team: "agents" },
    }),
    sandbox(MOCK_IDS.deletedSandbox, MOCK_IDS.buildBox, SandboxState.DELETED, {
      endReason: SandboxEndReason.IDLE_TTL_EXPIRED,
      endTime: ts(-HOUR),
    }),
    sandbox(
      MOCK_IDS.unreachableSandbox,
      MOCK_IDS.studioMac,
      SandboxState.UNREACHABLE,
      {
        lastKnownState: SandboxState.RUNNING,
        hostOs: HostOs.MACOS,
        arch: Arch.ARM64,
      },
    ),
  );
  acme.snapshots.push(
    create(SnapshotSchema, {
      snapshotId: MOCK_IDS.snapshot,
      organizationId: MOCK_IDS.acme,
      sourceSandboxId: MOCK_IDS.runningSandbox,
      deviceId: MOCK_IDS.buildBox,
      state: SnapshotState.READY,
      exportState: SnapshotExportState.NOT_EXPORTED,
      guestOs: GuestOs.LINUX,
      hostOs: HostOs.LINUX,
      arch: Arch.AMD64,
      resources: { vcpuCount: 2, memoryMib: 4096n, diskGib: 20n },
      hasMemoryState: true,
      sizeBytes: 3n * 1024n ** 3n,
      createTime: ts(-2 * HOUR),
    }),
  );
  acme.templates.push(
    create(TemplateSchema, {
      templateId: MOCK_IDS.template,
      organizationId: MOCK_IDS.acme,
      name: "node-24-toolchain",
      snapshotId: MOCK_IDS.snapshot,
      createTime: ts(-HOUR),
    }),
  );
  acme.tokens.push(
    create(EnrollmentTokenSchema, {
      enrollmentTokenId: newMockId(),
      organizationId: MOCK_IDS.acme,
      state: EnrollmentTokenState.USED,
      createTime: ts(-3 * 24 * HOUR),
      expireTime: ts(-2 * 24 * HOUR),
      useTime: ts(-3 * 24 * HOUR + 600),
      deviceId: MOCK_IDS.studioMac,
      tokenPrefix: "ose_91c2",
    }),
  );
  acme.apiKeys.push(
    create(ApiKeySchema, {
      apiKeyId: newMockId(),
      organizationId: MOCK_IDS.acme,
      name: "ci-runner",
      state: ApiKeyState.ACTIVE,
      createTime: ts(-10 * 24 * HOUR),
      keyPrefix: "osk_2b9d",
    }),
  );
  orgs.set(MOCK_IDS.acme, acme);

  const northwind = emptyOrg(MOCK_IDS.northwind, "Northwind Labs");
  northwind.members.push(
    member(MOCK_IDS.northwind, "user-owner-lee", Role.OWNER, "Seo-yeon Lee"),
    member(MOCK_IDS.northwind, MOCK_USER, Role.MEMBER, "Mina Kim"),
  );
  orgs.set(MOCK_IDS.northwind, northwind);

  const contoso = emptyOrg(MOCK_IDS.contoso, "Contoso Research");
  contoso.members.push(
    member(MOCK_IDS.contoso, "user-owner-ray", Role.OWNER, "Ray Choi"),
  );
  contoso.invitations.push({
    invitation: create(InvitationSchema, {
      invitationId: newMockId(),
      organizationId: MOCK_IDS.contoso,
      role: Role.MEMBER,
      state: InvitationState.PENDING,
      createTime: ts(-HOUR),
      expireTime: ts(24 * HOUR),
      secretPrefix: "osi_mock",
    }),
    secret: MOCK_INVITATION_SECRET,
  });
  orgs.set(MOCK_IDS.contoso, contoso);
  return orgs;
}

/** Options of a fake control plane, for tests. */
export interface MockTransportOptions {
  /** The signed-in user; a seeded member ID selects that member's role. */
  userId?: string;
  /** Billing states that replace the defaults of seeded organizations. */
  billing?: Readonly<Record<string, MockBillingState>>;
  /**
   * The billing accounts that ListLinkableBillingAccounts returns and that
   * the user can link, instead of MOCK_BILLING_ACCOUNTS.
   */
  linkableBillingAccounts?: readonly MockBillingAccount[];
  /**
   * Capabilities that replace the guest operating systems and the built base
   * images that seeded devices advertise, keyed by device ID. A field that is
   * absent keeps the seeded value. The seeded devices advertise only the guests
   * and images they can run, so a test uses this to add a device that can run
   * another guest.
   */
  deviceCapabilities?: Readonly<Record<string, MockDeviceCapabilities>>;
  /**
   * How many unused enrollment tokens to add to the Acme organization after
   * the seeded one, in listing order. A test uses this to fill more than one
   * page of 200 rows without a request for each row.
   */
  extraEnrollmentTokens?: number;
  /**
   * How many active API keys, named `key-0`, `key-1` and so on, to add to the
   * Acme organization after the seeded one, in listing order. A test uses
   * this to fill more than one page of 200 rows without a request for each
   * row.
   */
  extraApiKeys?: number;
  /** Pending invitations to add after the seeded Acme invitation. */
  extraInvitations?: number;
  /** Online Linux devices to add after the two seeded Acme devices. */
  extraDevices?: number;
  /** Ready snapshots to add after the seeded Acme snapshot. */
  extraSnapshots?: number;
  /** Templates to add after the seeded Acme template. */
  extraTemplates?: number;
  /**
   * How many running sandboxes, labelled `extra=<index>`, to add to the Acme
   * organization after the seeded ones, on the seeded `build-box` device with
   * the settings of the seeded running sandbox. A test uses this to fill more
   * than one page of sandboxes without a request for each row.
   */
  extraSandboxes?: number;
}

/** The capability fields of a seeded device that a fixture can replace. */
export interface MockDeviceCapabilities {
  guestOses?: readonly GuestOs[];
  builtBaseImages?: readonly BaseImage[];
}

/** The guest operating system that a base image boots. */
export function guestOsOfBaseImage(image: BaseImage): GuestOs {
  switch (image) {
    case BaseImage.LINUX:
      return GuestOs.LINUX;
    case BaseImage.MACOS:
      return GuestOs.MACOS;
    case BaseImage.WINDOWS:
      return GuestOs.WINDOWS;
    default:
      throw new ConnectError(
        "invalid source: the base image must be specified",
        Code.InvalidArgument,
      );
  }
}

/** What a device must offer to run a new sandbox. */
interface PlacementRequirement {
  guestOs: GuestOs;
  /** The base image of a base-image source; absent for a snapshot source. */
  baseImage?: BaseImage;
}

/** The permanent rule: whether the device advertises the guest and image. */
function deviceCanRun(device: Device, need: PlacementRequirement): boolean {
  const caps = device.capabilities;
  if (caps === undefined) return false;
  if (!caps.guestOses.includes(need.guestOs)) return false;
  return (
    need.baseImage === undefined ||
    caps.builtBaseImages.includes(need.baseImage)
  );
}

/**
 * Picks the device for a new sandbox like the control plane scheduler.
 *
 * A device can run a sandbox only when it advertises the guest operating
 * system of the source and, for a base-image source, has built that base
 * image. An explicit device ID is an exclusive selection: the request never
 * falls back to another device. An ID that names no visible device fails with
 * `NotFound` (a removed device is a tombstone, so it counts as not found), a
 * device that cannot run the source fails with `FailedPrecondition` (a
 * permanent rule, checked before the online state like the scheduler does),
 * and a device that is not `ONLINE` fails with `Unavailable`, the code for a
 * transient condition. Without an ID, the first `ONLINE` device that can run
 * the source runs the sandbox, and the result is `undefined` when none can.
 */
/**
 * Returns the request with every UUID field in lowercase text, like the real
 * API's canonicalization: the organization, the idempotency key, the explicit
 * placement device and the snapshot or template of the source. Other fields,
 * including user label text, are kept as sent.
 */
function canonicalizeCreateSandbox(
  req: CreateSandboxRequest,
): CreateSandboxRequest {
  const source = req.source?.source;
  return create(CreateSandboxRequestSchema, {
    ...req,
    organizationId: req.organizationId.toLowerCase(),
    idempotencyKey: req.idempotencyKey.toLowerCase(),
    source:
      source?.case === "snapshotId" || source?.case === "templateId"
        ? create(SandboxSourceSchema, {
            source: { case: source.case, value: source.value.toLowerCase() },
          })
        : req.source,
    placement:
      req.placement?.deviceId === undefined
        ? req.placement
        : { ...req.placement, deviceId: req.placement.deviceId.toLowerCase() },
  });
}

function placeSandbox(
  devices: Device[],
  deviceId: string | undefined,
  need: PlacementRequirement,
): Device | undefined {
  if (deviceId === undefined || deviceId === "") {
    return devices.find(
      (d) => d.state === DeviceState.ONLINE && deviceCanRun(d, need),
    );
  }
  const device = devices.find(
    (d) => d.deviceId === deviceId && d.state !== DeviceState.REMOVED,
  );
  if (device === undefined) {
    throw new ConnectError("device not found", Code.NotFound);
  }
  if (!deviceCanRun(device, need)) {
    throw new ConnectError(
      need.baseImage === undefined
        ? "the device does not support the guest OS"
        : "the device does not support the guest OS or has not built the base image",
      Code.FailedPrecondition,
    );
  }
  if (device.state !== DeviceState.ONLINE) {
    throw new ConnectError("the device is offline", Code.Unavailable);
  }
  return device;
}

const PAGE_TOKEN_PREFIX = "v1:";
const UUID_V7 =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

/** The timestamp of the seeded IDs, so that a created ID sorts after them. */
const SEEDED_ID_MS = 0x0193a0000000;
let lastIdMs = 0;
let idCounter = 0;

/**
 * A UUID v7 that is strictly greater than every ID the mock created before,
 * like the control plane generator (a 12-bit counter after the timestamp), so
 * that the ascending ID order of a list is the creation order even for rows
 * created in one millisecond. A clock that runs backwards, or before the
 * seeded IDs, does not move a created ID before an older one.
 */
function newMockId(): string {
  let ms = Math.max(Date.now(), lastIdMs, SEEDED_ID_MS + 1);
  if (ms === lastIdMs) {
    idCounter += 1;
    if (idCounter > 0xfff) {
      ms += 1;
      idCounter = 0;
    }
  } else {
    idCounter = 0;
  }
  lastIdMs = ms;
  const id = uuidV7(ms).split("");
  const counter = idCounter.toString(16).padStart(3, "0");
  // Characters 15..17 hold the 12 random bits after the version nibble (14).
  id.splice(15, 3, ...counter);
  return id.join("");
}

/**
 * One page of a list, like the control plane: rows in ascending ID order, 50
 * rows by default and at most 200 (a negative size fails with
 * `invalid_argument`), starting after the row ID that the opaque page token
 * names, and a continuation token whenever a page is full, even when no row
 * follows. The token is the unpadded base64url form of `v1:` and the ID of
 * the last row; a token that is not that form of a UUID v7 fails with
 * `invalid_argument`. A token names a position, not a row, so it keeps
 * working after the row it names is deleted.
 */
function pageOf<T>(
  rows: readonly T[],
  req: { pageSize: number; pageToken: string },
  idOf: (row: T) => string,
): { page: T[]; nextPageToken: string } {
  if (req.pageSize < 0) {
    throw new ConnectError("invalid page_size", Code.InvalidArgument);
  }
  const pageSize = req.pageSize === 0 ? 50 : Math.min(req.pageSize, 200);
  let after = "";
  if (req.pageToken !== "") {
    after = decodePageToken(req.pageToken);
  }
  const page = rows
    .filter((row) => after === "" || idOf(row).toLowerCase() > after)
    .sort((a, b) => {
      const x = idOf(a).toLowerCase();
      const y = idOf(b).toLowerCase();
      return x < y ? -1 : x > y ? 1 : 0;
    })
    .slice(0, pageSize);
  const last = page.at(-1);
  return {
    page,
    nextPageToken:
      page.length === pageSize && last !== undefined
        ? encodePageToken(idOf(last))
        : "",
  };
}

function encodePageToken(id: string): string {
  return btoa(PAGE_TOKEN_PREFIX + id.toLowerCase())
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

function decodePageToken(token: string): string {
  const invalid = new ConnectError("invalid page_token", Code.InvalidArgument);
  if (!/^[A-Za-z0-9_-]+$/.test(token)) throw invalid;
  let raw: string;
  try {
    raw = atob(token.replace(/-/g, "+").replace(/_/g, "/"));
  } catch {
    throw invalid;
  }
  if (!raw.startsWith(PAGE_TOKEN_PREFIX)) throw invalid;
  const after = raw.slice(PAGE_TOKEN_PREFIX.length);
  if (!UUID_V7.test(after)) throw invalid;
  return after.toLowerCase();
}

export function createMockTransport(
  options: MockTransportOptions = {},
): Transport {
  const userId = options.userId ?? MOCK_USER;
  const orgs = seed();
  for (const [orgId, state] of Object.entries({
    ...DEFAULT_BILLING,
    ...options.billing,
  })) {
    const org = orgs.get(orgId);
    if (org !== undefined) applyBillingState(org, state);
  }
  const acmeOrg = orgs.get(MOCK_IDS.acme);
  if (acmeOrg !== undefined) {
    const seededSnapshot = acmeOrg.snapshots[0]!;
    for (let i = 0; i < (options.extraSnapshots ?? 0); i++) {
      const snapshot = clone(SnapshotSchema, seededSnapshot);
      snapshot.snapshotId = newMockId();
      acmeOrg.snapshots.push(snapshot);
    }
    const seededTemplate = acmeOrg.templates[0]!;
    for (let i = 0; i < (options.extraTemplates ?? 0); i++) {
      const template = clone(TemplateSchema, seededTemplate);
      template.templateId = newMockId();
      template.name = `Template ${String(i + 2).padStart(3, "0")}`;
      acmeOrg.templates.push(template);
    }
    const seededInvitation = acmeOrg.invitations[0]!.invitation;
    for (let i = 0; i < (options.extraInvitations ?? 0); i++) {
      const invitation = clone(InvitationSchema, seededInvitation);
      invitation.invitationId = newMockId();
      invitation.secretPrefix = `osi_${String(i).padStart(4, "0")}`;
      acmeOrg.invitations.push({
        invitation,
        secret: `${invitation.secretPrefix}_unused`,
      });
    }
    for (let i = 0; i < (options.extraEnrollmentTokens ?? 0); i++) {
      acmeOrg.tokens.push(
        create(EnrollmentTokenSchema, {
          enrollmentTokenId: newMockId(),
          organizationId: MOCK_IDS.acme,
          state: EnrollmentTokenState.PENDING,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          expireTime: ts(HOUR),
          tokenPrefix: `ose_${String(i).padStart(4, "0")}`,
        }),
      );
    }
    const template = acmeOrg.sandboxes.find(
      (s) => s.sandboxId === MOCK_IDS.runningSandbox,
    );
    for (
      let i = 0;
      template !== undefined && i < (options.extraSandboxes ?? 0);
      i++
    ) {
      const extra = clone(SandboxSchema, template);
      extra.sandboxId = newMockId();
      extra.labels = { extra: String(i) };
      acmeOrg.sandboxes.push(extra);
    }
    const buildBox = acmeOrg.devices.find(
      (d) => d.deviceId === MOCK_IDS.buildBox,
    );
    for (
      let i = 0;
      buildBox !== undefined && i < (options.extraDevices ?? 0);
      i++
    ) {
      const device = clone(DeviceSchema, buildBox);
      device.deviceId = newMockId();
      device.name = `Device ${String(i + 3).padStart(3, "0")}`;
      device.allocation = undefined;
      acmeOrg.devices.push(device);
    }
    for (let i = 0; i < (options.extraApiKeys ?? 0); i++) {
      acmeOrg.apiKeys.push(
        create(ApiKeySchema, {
          apiKeyId: newMockId(),
          organizationId: MOCK_IDS.acme,
          name: `key-${i}`,
          state: ApiKeyState.ACTIVE,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          keyPrefix: `osk_${String(i).padStart(4, "0")}`,
        }),
      );
    }
  }
  for (const org of orgs.values()) {
    for (const device of org.devices) {
      const caps = options.deviceCapabilities?.[device.deviceId];
      if (caps === undefined || device.capabilities === undefined) continue;
      if (caps.guestOses !== undefined)
        device.capabilities.guestOses = [...caps.guestOses];
      if (caps.builtBaseImages !== undefined)
        device.capabilities.builtBaseImages = [...caps.builtBaseImages];
    }
  }
  const linkable = (
    options.linkableBillingAccounts ?? MOCK_BILLING_ACCOUNTS
  ).filter(
    (a) =>
      a.role === BillingAccountRole.OWNER ||
      a.role === BillingAccountRole.ADMIN,
  );
  // Copies, so a test's accounts are never changed.
  const billingAccounts = new Map<string, MockBillingAccount>(
    [...MOCK_BILLING_ACCOUNTS, ...(options.linkableBillingAccounts ?? [])].map(
      (a) => [a.billingAccountId, { ...a }],
    ),
  );

  /** The billing phase of an organization, computed like the control plane. */
  const billingOf = (org: OrgState): OrganizationBilling => {
    const { billingAccountId, blockedSinceMs } = org.billing;
    const account =
      billingAccountId === undefined
        ? undefined
        : billingAccounts.get(billingAccountId);
    const linked = account !== undefined;
    const paying =
      account !== undefined &&
      account.subscriptionActive &&
      account.balanceUsdMicros > 0n;
    if (paying) {
      return create(OrganizationBillingSchema, {
        phase: BillingPhase.ALLOWED,
        reason: BillingReason.UNSPECIFIED,
        linked: true,
      });
    }
    const since = blockedSinceMs ?? now();
    const suspended = now() >= since + SUSPEND_AFTER_MS;
    return create(OrganizationBillingSchema, {
      phase: suspended ? BillingPhase.SUSPENDED : BillingPhase.BLOCKED,
      reason: suspended
        ? BillingReason.SUSPENDED
        : linked
          ? BillingReason.BLOCKED
          : BillingReason.ACCOUNT_REQUIRED,
      linked,
      blockedSince: timestampFromMs(since),
      suspendTime: timestampFromMs(since + SUSPEND_AFTER_MS),
    });
  };
  /**
   * Billing enforcement of the resource-creating RPCs: a blocked or
   * suspended organization fails with resource_exhausted and an ErrorDetail
   * with the billing reason, like the control plane.
   */
  const requireBillingAllowed = (org: OrgState) => {
    const billing = billingOf(org);
    if (billing.phase === BillingPhase.ALLOWED) return;
    const message =
      billing.reason === BillingReason.ACCOUNT_REQUIRED
        ? `This organization has no linked billing account. Link one and pay at ${ACCOUNTS_URL}.`
        : billing.reason === BillingReason.BLOCKED
          ? `The billing account of this organization has no active subscription or no balance. Pay at ${ACCOUNTS_URL}.`
          : `This organization is suspended for non-payment. Pay at ${ACCOUNTS_URL}.`;
    throw new ConnectError(message, Code.ResourceExhausted, undefined, [
      {
        desc: ErrorDetailSchema,
        value: {
          code: ErrorCode.RESOURCE_EXHAUSTED,
          message,
          billingReason: billing.reason,
        },
      },
    ]);
  };
  /** Removes the billing link; the block starts now unless it started earlier. */
  const unlink = (org: OrgState) => {
    if (org.billing.billingAccountId === undefined) return;
    const wasPaying = billingOf(org).phase === BillingPhase.ALLOWED;
    org.billing.billingAccountId = undefined;
    if (wasPaying || org.billing.blockedSinceMs === undefined) {
      org.billing.blockedSinceMs = now();
    }
  };

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
  const withRole = (org: Organization, role: Role) =>
    create(OrganizationSchema, { ...org, callerRole: role });
  const findSandbox = (org: OrgState, id: string) => {
    const s = org.sandboxes.find((x) => x.sandboxId === id);
    if (s === undefined)
      throw new ConnectError("sandbox not found", Code.NotFound);
    return s;
  };
  const findDevice = (org: OrgState, id: string) => {
    const d = org.devices.find((x) => x.deviceId === id);
    if (d === undefined)
      throw new ConnectError("device not found", Code.NotFound);
    return d;
  };
  // Shared by the legacy and the strict limits RPC: the numeric rules and the
  // derived effective vCPU limit of the server.
  const applyDeviceLimits = (
    org: OrgState,
    deviceId: string,
    l: DeviceLimits | undefined,
  ) => {
    if (
      l === undefined ||
      l.vcpuCount < 0 ||
      l.memoryMib < 0n ||
      l.diskGib < 0n
    ) {
      throw new ConnectError(
        "limits must not be negative",
        Code.InvalidArgument,
      );
    }
    const p = l.vcpuOvercommitPercent;
    if (p < 100 || p > 400 || p % 25 !== 0) {
      throw new ConnectError(
        "limits.vcpu_overcommit_percent must be 100 to 400 in steps of 25",
        Code.InvalidArgument,
      );
    }
    const device = findDevice(org, deviceId);
    if (device.state === DeviceState.REMOVED) {
      throw new ConnectError("device not found", Code.NotFound);
    }
    device.limits = l;
    // Like the server: the exact int64 value, and the int32 field capped.
    const effective = effectiveVcpuLimit(l.vcpuCount, p);
    device.effectiveVcpuLimitCount = BigInt(effective);
    device.effectiveVcpuLimit = Math.min(effective, MAX_INT32);
    return device;
  };
  const findSnapshot = (org: OrgState, id: string) => {
    const s = org.snapshots.find((x) => x.snapshotId === id);
    if (s === undefined)
      throw new ConnectError("snapshot not found", Code.NotFound);
    return s;
  };
  /**
   * The guest operating system of a sandbox source, like the control plane: a
   * base image boots its own guest, a snapshot keeps the guest of its sandbox,
   * and a template has the guest of its snapshot. `baseImage` is set for a
   * base-image source only, because only that source needs a built image.
   */
  const sourceGuest = (
    org: OrgState,
    source: SandboxSource | undefined,
  ): PlacementRequirement => {
    switch (source?.source.case) {
      case "baseImage":
        return {
          guestOs: guestOsOfBaseImage(source.source.value),
          baseImage: source.source.value,
        };
      case "snapshotId":
        return { guestOs: findSnapshot(org, source.source.value).guestOs };
      case "templateId": {
        const id = source.source.value;
        const template = org.templates.find((t) => t.templateId === id);
        if (template === undefined)
          throw new ConnectError("template not found", Code.NotFound);
        return { guestOs: findSnapshot(org, template.snapshotId).guestOs };
      }
      default:
        throw new ConnectError(
          "invalid source: set a base image, a snapshot or a template",
          Code.InvalidArgument,
        );
    }
  };
  const secret = (prefix: string) =>
    `${prefix}_${Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
      b.toString(16).padStart(2, "0"),
    ).join("")}`;

  return createRouterTransport((router) => {
    router.service(OrganizationService, {
      createOrganization(req) {
        const name = req.name.trim();
        if (name === "")
          throw new ConnectError("name is required", Code.InvalidArgument);
        const id = newMockId();
        const org = emptyOrg(id, name);
        org.members.push(member(id, userId, Role.OWNER, "Mina Kim"));
        orgs.set(id, org);
        return { organization: withRole(org.organization, Role.OWNER) };
      },
      getOrganization(req) {
        const { org, role } = membership(req.organizationId);
        return { organization: withRole(org.organization, role) };
      },
      listOrganizations(req) {
        const all = [...orgs.values()]
          .map((org) => ({
            org,
            m: org.members.find((x) => x.userId === userId),
          }))
          .filter((x) => x.m !== undefined)
          .map((x) => withRole(x.org.organization, x.m!.role));
        const { page, nextPageToken } = pageOf(
          all,
          req,
          (o) => o.organizationId,
        );
        return { organizations: page, nextPageToken };
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
        // Like the control plane, the owner cannot transfer to themselves;
        // this runs before any lookup or change.
        if (req.userId === userId) {
          throw new ConnectError(
            "invalid user_id: the caller is already the owner",
            Code.InvalidArgument,
          );
        }
        const target = org.members.find((m) => m.userId === req.userId);
        if (target === undefined)
          throw new ConnectError("member not found", Code.NotFound);
        for (const m of org.members) {
          if (m.userId === userId) m.role = Role.ADMIN;
        }
        target.role = Role.OWNER;
        // An ownership transfer removes the billing link.
        unlink(org);
        return {};
      },
    });

    router.service(MemberService, {
      createInvitation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (req.role !== Role.ADMIN && req.role !== Role.MEMBER) {
          throw new ConnectError(
            "invalid role: must be ROLE_ADMIN or ROLE_MEMBER; use TransferOwnership for the owner",
            Code.InvalidArgument,
          );
        }
        const value = secret("osi");
        const invitation = create(InvitationSchema, {
          invitationId: newMockId(),
          organizationId: req.organizationId,
          role: req.role,
          state: InvitationState.PENDING,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          expireTime: ts(7 * 24 * HOUR),
          secretPrefix: value.slice(0, 8),
        });
        org.invitations.push({ invitation, secret: value });
        return { invitation, invitationSecret: value };
      },
      listInvitations(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const cutoff = now();
        const invitations = org.invitations
          .map(({ invitation }) => invitation)
          .filter((invitation) => isPendingInvitation(invitation, cutoff));
        const { page, nextPageToken } = pageOf(
          invitations,
          req,
          (invitation) => invitation.invitationId,
        );
        return { invitations: page, nextPageToken };
      },
      revokeInvitation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const entry = org.invitations.find(
          (i) => i.invitation.invitationId === req.invitationId,
        );
        if (entry === undefined)
          throw new ConnectError("invitation not found", Code.NotFound);
        if (!isPendingInvitation(entry.invitation, now())) {
          throw new ConnectError(
            "only a pending invitation can be revoked",
            Code.FailedPrecondition,
          );
        }
        entry.invitation.state = InvitationState.REVOKED;
        return { invitation: entry.invitation };
      },
      acceptInvitation(req) {
        for (const org of orgs.values()) {
          const entry = org.invitations.find(
            (i) =>
              i.secret === req.invitationSecret &&
              i.invitation.state === InvitationState.PENDING,
          );
          if (entry === undefined) continue;
          // Like the control plane, an invitation at or after its expiry is
          // indistinguishable from an unknown secret, and the check runs
          // before the membership check.
          const { expireTime } = entry.invitation;
          if (expireTime !== undefined && now() >= timestampMs(expireTime))
            continue;
          if (org.members.some((m) => m.userId === userId)) {
            throw new ConnectError(
              "you are already a member",
              Code.AlreadyExists,
            );
          }
          entry.invitation.state = InvitationState.ACCEPTED;
          const m = member(
            org.organization.organizationId,
            userId,
            entry.invitation.role,
            "Mina Kim",
          );
          org.members.push(m);
          return {
            organization: withRole(org.organization, m.role),
            member: m,
          };
        }
        throw new ConnectError("invitation not found", Code.NotFound);
      },
      listMembers(req) {
        const { org } = membership(req.organizationId);
        return { members: org.members };
      },
      updateMemberRole(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        // Mirrors the real API: only admin and member can be assigned. An
        // omitted, unspecified, unknown or owner role is rejected before the
        // member lookup, so nothing is read or changed.
        if (req.role !== Role.ADMIN && req.role !== Role.MEMBER) {
          throw new ConnectError(
            "invalid role: must be ROLE_ADMIN or ROLE_MEMBER; use TransferOwnership for the owner",
            Code.InvalidArgument,
          );
        }
        const target = org.members.find((m) => m.userId === req.userId);
        if (target === undefined)
          throw new ConnectError("member not found", Code.NotFound);
        if (target.role === Role.OWNER) {
          throw new ConnectError(
            "use ownership transfer for the owner",
            Code.PermissionDenied,
          );
        }
        target.role = req.role;
        return { member: target };
      },
      removeMember(req) {
        const { org, role } = membership(req.organizationId);
        if (req.userId === userId) {
          if (role === Role.OWNER) {
            throw new ConnectError(
              "the owner cannot leave",
              Code.FailedPrecondition,
            );
          }
        } else {
          requireAdmin(role);
        }
        if (req.userId === "") {
          throw new ConnectError(
            "invalid user_id: is required",
            Code.InvalidArgument,
          );
        }
        const target = org.members.find((m) => m.userId === req.userId);
        if (target === undefined) {
          throw new ConnectError("member not found", Code.NotFound);
        }
        if (target.role === Role.OWNER) {
          throw new ConnectError(
            "the owner cannot be removed",
            Code.PermissionDenied,
          );
        }
        org.members = org.members.filter((m) => m.userId !== req.userId);
        return {};
      },
    });

    router.service(DeviceService, {
      createEnrollmentToken(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        requireBillingAllowed(org);
        const ttl = Number(req.ttl?.seconds ?? 0n);
        if (ttl <= 0 || ttl > 24 * HOUR) {
          throw new ConnectError(
            "ttl must be at most 24 hours",
            Code.InvalidArgument,
          );
        }
        const value = secret("ose");
        const token = create(EnrollmentTokenSchema, {
          enrollmentTokenId: newMockId(),
          organizationId: req.organizationId,
          state: EnrollmentTokenState.PENDING,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          expireTime: ts(ttl),
          tokenPrefix: value.slice(0, 8),
        });
        // The control plane lists in ascending ID order (UUID v7), so a new
        // token is last.
        org.tokens.push(token);
        return { token, enrollmentToken: value };
      },
      listEnrollmentTokens(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        // The control plane derives EXPIRED when it reads an unused, unrevoked
        // token at or after its expiry. The stored record stays PENDING, so the
        // list returns a copy and a later revoke still finds a pending token.
        const at = now();
        const { page, nextPageToken } = pageOf(
          org.tokens,
          req,
          (t) => t.enrollmentTokenId,
        );
        return {
          tokens: page.map((t) =>
            t.state === EnrollmentTokenState.PENDING &&
            t.expireTime !== undefined &&
            at >= timestampMs(t.expireTime)
              ? (() => {
                  const expired = clone(EnrollmentTokenSchema, t);
                  expired.state = EnrollmentTokenState.EXPIRED;
                  return expired;
                })()
              : t,
          ),
          nextPageToken,
        };
      },
      revokeEnrollmentToken(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const token = org.tokens.find(
          (t) => t.enrollmentTokenId === req.enrollmentTokenId,
        );
        if (token === undefined)
          throw new ConnectError("token not found", Code.NotFound);
        // Mirrors the real API: revoking a revoked token returns it unchanged.
        if (token.state === EnrollmentTokenState.REVOKED) return { token };
        if (token.state !== EnrollmentTokenState.PENDING) {
          throw new ConnectError(
            "only an unused token can be revoked",
            Code.FailedPrecondition,
          );
        }
        token.state = EnrollmentTokenState.REVOKED;
        return { token };
      },
      listDevices(req) {
        const { org } = membership(req.organizationId);
        const { state } = req;
        if (
          state !== undefined &&
          (state === DeviceState.UNSPECIFIED ||
            !DeviceStateSchema.values.some((value) => value.number === state))
        ) {
          throw new ConnectError(
            "invalid state: must be a device state",
            Code.InvalidArgument,
          );
        }
        const devices = org.devices.filter((d) =>
          state === undefined
            ? d.state !== DeviceState.REMOVED
            : d.state === state,
        );
        const { page, nextPageToken } = pageOf(devices, req, (d) => d.deviceId);
        return { devices: page, nextPageToken };
      },
      getDevice(req) {
        const { org } = membership(req.organizationId);
        return { device: findDevice(org, req.deviceId) };
      },
      updateDeviceLimits(req) {
        // Legacy RPC: a missing value reads as zero.
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        return { device: applyDeviceLimits(org, req.deviceId, req.limits) };
      },
      replaceDeviceLimits(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const l = req.limits;
        // Like the server: every value must be present, and an explicit zero
        // is a value. The first missing field in schema order is named.
        const missing =
          l === undefined
            ? "limits"
            : l.vcpuCount === undefined
              ? "limits.vcpu_count"
              : l.memoryMib === undefined
                ? "limits.memory_mib"
                : l.diskGib === undefined
                  ? "limits.disk_gib"
                  : l.vcpuOvercommitPercent === undefined
                    ? "limits.vcpu_overcommit_percent"
                    : undefined;
        if (l === undefined || missing !== undefined) {
          throw new ConnectError(
            `${missing} is required`,
            Code.InvalidArgument,
          );
        }
        return {
          device: applyDeviceLimits(
            org,
            req.deviceId,
            create(DeviceLimitsSchema, {
              vcpuCount: l.vcpuCount,
              memoryMib: l.memoryMib,
              diskGib: l.diskGib,
              vcpuOvercommitPercent: l.vcpuOvercommitPercent,
            }),
          ),
        };
      },
      removeDevice(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const previous = findDevice(org, req.deviceId);
        const device = create(DeviceSchema, {
          deviceId: previous.deviceId,
          organizationId: previous.organizationId,
          createTime: previous.createTime,
          state: DeviceState.REMOVED,
          limits: { vcpuOvercommitPercent: 100 },
          allocation: { vcpuTotalCount: 0n },
        });
        org.devices[org.devices.indexOf(previous)] = device;
        const endTime = ts();
        for (const s of org.sandboxes) {
          if (
            s.deviceId === device.deviceId &&
            s.state !== SandboxState.DELETED
          ) {
            s.state = SandboxState.DELETED;
            s.endReason = SandboxEndReason.DEVICE_REMOVED;
            s.endTime = endTime;
          }
        }
        // Like the real control plane, delete the records of snapshots that
        // exist only on the removed device, and the templates that reference
        // them. An exported snapshot is organization data: it stays, without
        // a local copy.
        const dropped = new Set(
          org.snapshots
            .filter(
              (s) =>
                s.deviceId === device.deviceId &&
                s.exportState !== SnapshotExportState.EXPORTED,
            )
            .map((s) => s.snapshotId),
        );
        org.snapshots = org.snapshots.filter((s) => !dropped.has(s.snapshotId));
        org.templates = org.templates.filter((t) => !dropped.has(t.snapshotId));
        for (const s of org.snapshots) {
          if (s.deviceId === device.deviceId) s.deviceId = "";
        }
        return { device };
      },
      getAgentVersionPolicy(req) {
        const { org } = membership(req.organizationId);
        return { policy: org.policy };
      },
      updateAgentVersionPolicy(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (
          req.mode === AgentVersionPolicyMode.PIN &&
          req.pinnedVersion.trim() === ""
        ) {
          throw new ConnectError(
            "pinned_version is required",
            Code.InvalidArgument,
          );
        }
        org.policy = create(AgentVersionPolicySchema, {
          organizationId: req.organizationId,
          mode: req.mode,
          pinnedVersion:
            req.mode === AgentVersionPolicyMode.PIN ? req.pinnedVersion : "",
          updateTime: ts(),
          updatedBy: { kind: PrincipalKind.USER, id: userId },
        });
        return { policy: org.policy };
      },
    });

    router.service(ApiKeyService, {
      createApiKey(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (req.name.trim() === "")
          throw new ConnectError("name is required", Code.InvalidArgument);
        const value = secret("osk");
        const apiKey = create(ApiKeySchema, {
          apiKeyId: newMockId(),
          organizationId: req.organizationId,
          name: req.name.trim(),
          state: ApiKeyState.ACTIVE,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          keyPrefix: value.slice(0, 8),
        });
        // The control plane lists in ascending ID order (UUID v7), so a new key
        // is last.
        org.apiKeys.push(apiKey);
        return { apiKey, apiKeySecret: value };
      },
      listApiKeys(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const { page, nextPageToken } = pageOf(
          org.apiKeys,
          req,
          (k) => k.apiKeyId,
        );
        return { apiKeys: page, nextPageToken };
      },
      revokeApiKey(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const key = org.apiKeys.find((k) => k.apiKeyId === req.apiKeyId);
        if (key === undefined)
          throw new ConnectError("API key not found", Code.NotFound);
        if (key.state === ApiKeyState.REVOKED) return { apiKey: key };
        key.state = ApiKeyState.REVOKED;
        key.revokeTime = ts();
        return { apiKey: key };
      },
    });

    router.service(SandboxService, {
      createSandbox(rawReq) {
        // Same as the real API: spell every UUID of the request in lowercase
        // before anything uses it, so two hex cases of one request name one
        // organization, one device, one source and one idempotency key.
        const req = canonicalizeCreateSandbox(rawReq);
        const { org } = membership(req.organizationId);
        requireBillingAllowed(org);
        const idleTtlNanos = ttlNanos(req.idleTtl, "idle_ttl");
        const maxTtlNanos = ttlNanos(req.maxTtl, "max_ttl");
        if (maxTtlNanos > MAX_TTL_NANOS) {
          throw new ConnectError(
            "max_ttl must be at most 7 days",
            Code.InvalidArgument,
          );
        }
        if (idleTtlNanos > maxTtlNanos) {
          throw new ConnectError(
            "idle_ttl must not be greater than max_ttl",
            Code.InvalidArgument,
          );
        }
        // Map entries serialize in insertion order, so sort labels first to make
        // the comparison independent of label order (the real API hashes a
        // deterministic encoding).
        const canonical = create(CreateSandboxRequestSchema, {
          ...req,
          labels: Object.fromEntries(
            Object.entries(req.labels).sort(([a], [b]) =>
              a < b ? -1 : a > b ? 1 : 0,
            ),
          ),
        });
        const request = Array.from(
          toBinary(CreateSandboxRequestSchema, canonical),
          (b) => b.toString(16).padStart(2, "0"),
        ).join("");
        const nowMs = now();
        const known = org.createKeys.get(req.idempotencyKey);
        if (known !== undefined && known.expiresAtMs > nowMs) {
          // Same as the real API: a known key bound to other parameters fails.
          if (known.request !== request) {
            throw new ConnectError(
              "the idempotency_key was used with other parameters",
              Code.AlreadyExists,
            );
          }
          return { sandbox: findSandbox(org, known.sandboxId) };
        }
        const { guestOs, baseImage } = sourceGuest(org, req.source);
        const device = placeSandbox(org.devices, req.placement?.deviceId, {
          guestOs,
          baseImage,
        });
        if (device === undefined) {
          throw new ConnectError(
            "no device can run this sandbox",
            Code.FailedPrecondition,
          );
        }
        const createTime = timestampFromMs(nowMs);
        const sandbox = create(SandboxSchema, {
          sandboxId: newMockId(),
          organizationId: req.organizationId,
          deviceId: device.deviceId,
          state: SandboxState.RUNNING,
          source: req.source,
          resources: req.resources,
          hostOs: device.hostOs,
          arch: device.arch,
          guestOs,
          network: req.network ?? { mode: NetworkMode.OUTBOUND },
          labels: { ...req.labels },
          idleTtl: req.idleTtl,
          maxTtl: req.maxTtl,
          idleDeadline: addTtl(createTime, idleTtlNanos),
          maxDeadline: addTtl(createTime, maxTtlNanos),
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime,
        });
        org.sandboxes.unshift(sandbox);
        org.createKeys.set(req.idempotencyKey, {
          sandboxId: sandbox.sandboxId,
          request,
          expiresAtMs: nowMs + DAY_MS,
        });
        return { sandbox };
      },
      getSandbox(req) {
        const { org } = membership(req.organizationId);
        const s = findSandbox(org, req.sandboxId);
        return { sandbox: s };
      },
      listSandboxes(req) {
        const { org } = membership(req.organizationId);
        const ended = (s: Sandbox) =>
          s.state === SandboxState.DELETED || s.state === SandboxState.FAILED;
        const filtered = org.sandboxes
          .filter(
            (s) => req.includeEnded || req.state !== undefined || !ended(s),
          )
          .filter((s) => req.state === undefined || s.state === req.state)
          .filter(
            (s) => req.deviceId === undefined || s.deviceId === req.deviceId,
          );
        const { page, nextPageToken } = pageOf(
          filtered,
          req,
          (s) => s.sandboxId,
        );
        return { sandboxes: page, nextPageToken };
      },
      deleteSandbox(req) {
        const { org } = membership(req.organizationId);
        const s = findSandbox(org, req.sandboxId);
        if (
          s.state !== SandboxState.DELETED &&
          s.state !== SandboxState.FAILED
        ) {
          s.state = SandboxState.DELETED;
          s.endReason = SandboxEndReason.DELETED_BY_CALLER;
          s.endTime = ts();
        }
        return { sandbox: s };
      },
      pauseSandbox(req) {
        const { org } = membership(req.organizationId);
        const s = findSandbox(org, req.sandboxId);
        if (
          s.state !== SandboxState.RUNNING &&
          s.state !== SandboxState.PAUSED
        ) {
          throw new ConnectError(
            "the sandbox is not running",
            Code.FailedPrecondition,
          );
        }
        s.state = SandboxState.PAUSED;
        return { sandbox: s };
      },
      resumeSandbox(req) {
        const { org } = membership(req.organizationId);
        requireBillingAllowed(org);
        const s = findSandbox(org, req.sandboxId);
        if (
          s.state !== SandboxState.RUNNING &&
          s.state !== SandboxState.PAUSED
        ) {
          throw new ConnectError(
            "the sandbox is not paused",
            Code.FailedPrecondition,
          );
        }
        s.state = SandboxState.RUNNING;
        return { sandbox: s };
      },
    });

    router.service(SnapshotService, {
      createSnapshot(req) {
        const { org } = membership(req.organizationId);
        requireBillingAllowed(org);
        const s = findSandbox(org, req.sandboxId);
        if (
          s.state !== SandboxState.RUNNING &&
          s.state !== SandboxState.PAUSED
        ) {
          throw new ConnectError(
            "the sandbox is not running or paused",
            Code.FailedPrecondition,
          );
        }
        const snapshot = create(SnapshotSchema, {
          snapshotId: newMockId(),
          organizationId: req.organizationId,
          sourceSandboxId: s.sandboxId,
          deviceId: s.deviceId,
          state: SnapshotState.READY,
          exportState: SnapshotExportState.NOT_EXPORTED,
          guestOs: s.guestOs,
          hostOs: s.hostOs,
          arch: s.arch,
          resources: s.resources,
          sizeBytes: 2n * 1024n ** 3n,
          createTime: ts(),
        });
        org.snapshots.unshift(snapshot);
        return { snapshot };
      },
      getSnapshot(req) {
        const { org } = membership(req.organizationId);
        return { snapshot: findSnapshot(org, req.snapshotId) };
      },
      listSnapshots(req) {
        const { org } = membership(req.organizationId);
        const filtered = org.snapshots
          .filter(
            (s) =>
              req.sandboxId === undefined ||
              s.sourceSandboxId === req.sandboxId,
          )
          .filter(
            (s) => req.deviceId === undefined || s.deviceId === req.deviceId,
          );
        const { page, nextPageToken } = pageOf(
          filtered,
          req,
          (s) => s.snapshotId,
        );
        return { snapshots: page, nextPageToken };
      },
      deleteSnapshot(req) {
        const { org } = membership(req.organizationId);
        findSnapshot(org, req.snapshotId);
        if (org.templates.some((t) => t.snapshotId === req.snapshotId)) {
          throw new ConnectError(
            "a template references this snapshot; delete the template first",
            Code.FailedPrecondition,
          );
        }
        org.snapshots = org.snapshots.filter(
          (s) => s.snapshotId !== req.snapshotId,
        );
        return {};
      },
      exportSnapshot(req) {
        const { org } = membership(req.organizationId);
        requireBillingAllowed(org);
        const s = findSnapshot(org, req.snapshotId);
        s.exportState = SnapshotExportState.EXPORTED;
        return { snapshot: s };
      },
      createTemplate(req) {
        const { org } = membership(req.organizationId);
        requireBillingAllowed(org);
        findSnapshot(org, req.snapshotId);
        if (req.name.trim() === "")
          throw new ConnectError("name is required", Code.InvalidArgument);
        const template = create(TemplateSchema, {
          templateId: newMockId(),
          organizationId: req.organizationId,
          name: req.name.trim(),
          snapshotId: req.snapshotId,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
        });
        org.templates.unshift(template);
        return { template };
      },
      listTemplates(req) {
        const { org } = membership(req.organizationId);
        const { page, nextPageToken } = pageOf(
          org.templates,
          req,
          (t) => t.templateId,
        );
        return { templates: page, nextPageToken };
      },
      deleteTemplate(req) {
        const { org } = membership(req.organizationId);
        org.templates = org.templates.filter(
          (t) => t.templateId !== req.templateId,
        );
        return {};
      },
    });

    router.service(BillingService, {
      getBillingStatus(req) {
        const { org, role } = membership(req.organizationId);
        return {
          billing: billingOf(org),
          billingAccountId:
            role === Role.OWNER ? org.billing.billingAccountId : undefined,
        };
      },
      getBillingUsage(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const account =
          org.billing.billingAccountId === undefined
            ? undefined
            : billingAccounts.get(org.billing.billingAccountId);
        if (account === undefined) {
          throw new ConnectError(
            "the organization has no linked billing account",
            Code.FailedPrecondition,
          );
        }
        const today = new Date(now());
        return {
          billingAccountId: account.billingAccountId,
          billingAccountName: account.name,
          balanceUsdMicros: account.balanceUsdMicros,
          monthStart: timestampFromMs(
            Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), 1),
          ),
          meterUsages: org.billing.usage,
        };
      },
      listLinkableBillingAccounts(req) {
        const { role } = membership(req.organizationId);
        requireOwner(role);
        return {
          billingAccounts: linkable.map((a) =>
            create(LinkableBillingAccountSchema, {
              billingAccountId: a.billingAccountId,
              name: a.name,
              role: a.role,
            }),
          ),
        };
      },
      linkBillingAccount(req) {
        const { org, role } = membership(req.organizationId);
        requireOwner(role);
        const account = linkable.find(
          (a) => a.billingAccountId === req.billingAccountId,
        );
        if (account === undefined) {
          throw new ConnectError("billing account not found", Code.NotFound);
        }
        const current = org.billing.billingAccountId;
        if (current !== undefined && current !== account.billingAccountId) {
          throw new ConnectError(
            "the organization is linked to another billing account; unlink it first",
            Code.AlreadyExists,
          );
        }
        org.billing.billingAccountId = account.billingAccountId;
        const billing = billingOf(org);
        // A paying account ends the non-payment; otherwise the earlier
        // block start is kept, so relinking never restarts the 3 days.
        if (billing.phase === BillingPhase.ALLOWED) {
          org.billing.blockedSinceMs = undefined;
        }
        return { billing, billingAccountName: account.name };
      },
      unlinkBillingAccount(req) {
        const { org, role } = membership(req.organizationId);
        requireOwner(role);
        if (
          req.expectedBillingAccountId !== undefined &&
          req.expectedBillingAccountId.toLowerCase() !==
            org.billing.billingAccountId
        ) {
          throw new ConnectError(
            "the linked billing account changed; review the current account and confirm again",
            Code.FailedPrecondition,
          );
        }
        unlink(org);
        return { billing: billingOf(org) };
      },
    });
  });
}
