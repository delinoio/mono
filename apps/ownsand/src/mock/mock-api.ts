// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type Duration,
  type Timestamp,
  timestampFromMs,
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
} from "ownsand-sdk/gen/api_key_pb";
import {
  AgentUpdateState,
  AgentVersionPolicyMode,
  ApiKeyState,
  Arch,
  BaseImage,
  DeviceState,
  EnrollmentTokenState,
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
} from "ownsand-sdk/gen/common_pb";
import {
  type AgentVersionPolicy,
  AgentVersionPolicySchema,
  type Device,
  DeviceSchema,
  DeviceService,
  type EnrollmentToken,
  EnrollmentTokenSchema,
} from "ownsand-sdk/gen/device_pb";
import {
  type Invitation,
  InvitationSchema,
  type Member,
  MemberSchema,
  MemberService,
} from "ownsand-sdk/gen/member_pb";
import {
  type Organization,
  OrganizationSchema,
  OrganizationService,
} from "ownsand-sdk/gen/organization_pb";
import {
  type Sandbox,
  SandboxSchema,
  SandboxService,
} from "ownsand-sdk/gen/sandbox_pb";
import {
  type Snapshot,
  SnapshotSchema,
  SnapshotService,
  type Template,
  TemplateSchema,
} from "ownsand-sdk/gen/snapshot_pb";
import { effectiveVcpuLimit, MAX_INT32 } from "../format.ts";
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
} as const;

const HOUR = 3600;
const now = () => Date.now();
const ts = (offsetSeconds = 0): Timestamp =>
  timestampFromMs(now() + offsetSeconds * 1000);
const dur = (seconds: number): Duration =>
  ({ seconds: BigInt(seconds), nanos: 0 }) as Duration;

interface OrgState {
  organization: Organization;
  members: Member[];
  invitations: { invitation: Invitation; secret: string }[];
  devices: Device[];
  tokens: EnrollmentToken[];
  apiKeys: ApiKey[];
  sandboxes: Sandbox[];
  snapshots: Snapshot[];
  templates: Template[];
  policy: AgentVersionPolicy;
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
    snapshots: [],
    templates: [],
    policy: create(AgentVersionPolicySchema, {
      organizationId: id,
      mode: AgentVersionPolicyMode.FOLLOW_DEFAULT,
    }),
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
      invitationId: uuidV7(),
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
      allocation: { vcpuCount: 10, memoryMib: 20480n, diskGib: 80n },
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
      allocation: { vcpuCount: 2, memoryMib: 4096n, diskGib: 20n },
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
      enrollmentTokenId: uuidV7(),
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
      apiKeyId: uuidV7(),
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
      invitationId: uuidV7(),
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

/** Creates a transport backed by a fresh fake control plane. */
export function createMockTransport(
  options: { userId?: string } = {},
): Transport {
  const userId = options.userId ?? MOCK_USER;
  const orgs = seed();

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
  const findSnapshot = (org: OrgState, id: string) => {
    const s = org.snapshots.find((x) => x.snapshotId === id);
    if (s === undefined)
      throw new ConnectError("snapshot not found", Code.NotFound);
    return s;
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
        const id = uuidV7();
        const org = emptyOrg(id, name);
        org.members.push(member(id, userId, Role.OWNER, "Mina Kim"));
        orgs.set(id, org);
        return { organization: withRole(org.organization, Role.OWNER) };
      },
      getOrganization(req) {
        const { org, role } = membership(req.organizationId);
        return { organization: withRole(org.organization, role) };
      },
      listOrganizations() {
        const organizations = [...orgs.values()]
          .map((org) => ({
            org,
            m: org.members.find((x) => x.userId === userId),
          }))
          .filter((x) => x.m !== undefined)
          .map((x) => withRole(x.org.organization, x.m!.role));
        return { organizations };
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
        if (target === undefined)
          throw new ConnectError("member not found", Code.NotFound);
        for (const m of org.members) {
          if (m.userId === userId) m.role = Role.ADMIN;
        }
        target.role = Role.OWNER;
        return {};
      },
    });

    router.service(MemberService, {
      createInvitation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        if (req.role !== Role.ADMIN && req.role !== Role.MEMBER) {
          throw new ConnectError(
            "role must be admin or member",
            Code.InvalidArgument,
          );
        }
        const value = secret("osi");
        const invitation = create(InvitationSchema, {
          invitationId: uuidV7(),
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
        return { invitations: org.invitations.map((i) => i.invitation) };
      },
      revokeInvitation(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const entry = org.invitations.find(
          (i) => i.invitation.invitationId === req.invitationId,
        );
        if (entry === undefined)
          throw new ConnectError("invitation not found", Code.NotFound);
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
        const target = org.members.find((m) => m.userId === req.userId);
        if (target === undefined)
          throw new ConnectError("member not found", Code.NotFound);
        if (target.role === Role.OWNER || req.role === Role.OWNER) {
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
          const target = org.members.find((m) => m.userId === req.userId);
          if (target?.role === Role.OWNER) {
            throw new ConnectError(
              "the owner cannot be removed",
              Code.PermissionDenied,
            );
          }
        }
        org.members = org.members.filter((m) => m.userId !== req.userId);
        return {};
      },
    });

    router.service(DeviceService, {
      createEnrollmentToken(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const ttl = Number(req.ttl?.seconds ?? 0n);
        if (ttl <= 0 || ttl > 24 * HOUR) {
          throw new ConnectError(
            "ttl must be at most 24 hours",
            Code.InvalidArgument,
          );
        }
        const value = secret("ose");
        const token = create(EnrollmentTokenSchema, {
          enrollmentTokenId: uuidV7(),
          organizationId: req.organizationId,
          state: EnrollmentTokenState.PENDING,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          expireTime: ts(ttl),
          tokenPrefix: value.slice(0, 8),
        });
        org.tokens.unshift(token);
        return { token, enrollmentToken: value };
      },
      listEnrollmentTokens(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        return { tokens: org.tokens };
      },
      revokeEnrollmentToken(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const token = org.tokens.find(
          (t) => t.enrollmentTokenId === req.enrollmentTokenId,
        );
        if (token === undefined)
          throw new ConnectError("token not found", Code.NotFound);
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
        // Mirrors the real API: an absent state means every device that is not
        // removed, and an explicit UNSPECIFIED state is invalid.
        if (req.state === DeviceState.UNSPECIFIED) {
          throw new ConnectError(
            "invalid state: must be a device state",
            Code.InvalidArgument,
          );
        }
        return {
          devices: org.devices.filter((d) =>
            req.state === undefined
              ? d.state !== DeviceState.REMOVED
              : d.state === req.state,
          ),
        };
      },
      getDevice(req) {
        const { org } = membership(req.organizationId);
        return { device: findDevice(org, req.deviceId) };
      },
      updateDeviceLimits(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const device = findDevice(org, req.deviceId);
        const l = req.limits;
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
        device.limits = l;
        // Like the server: the exact int64 value, and the int32 field capped.
        const effective = effectiveVcpuLimit(l.vcpuCount, p);
        device.effectiveVcpuLimitCount = BigInt(effective);
        device.effectiveVcpuLimit = Math.min(effective, MAX_INT32);
        return { device };
      },
      removeDevice(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const device = findDevice(org, req.deviceId);
        device.state = DeviceState.REMOVED;
        for (const s of org.sandboxes) {
          if (
            s.deviceId === device.deviceId &&
            s.state !== SandboxState.DELETED
          ) {
            s.state = SandboxState.DELETED;
            s.endReason = SandboxEndReason.DEVICE_REMOVED;
          }
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
          apiKeyId: uuidV7(),
          organizationId: req.organizationId,
          name: req.name.trim(),
          state: ApiKeyState.ACTIVE,
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
          keyPrefix: value.slice(0, 8),
        });
        org.apiKeys.unshift(apiKey);
        return { apiKey, apiKeySecret: value };
      },
      listApiKeys(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        return { apiKeys: org.apiKeys };
      },
      revokeApiKey(req) {
        const { org, role } = membership(req.organizationId);
        requireAdmin(role);
        const key = org.apiKeys.find((k) => k.apiKeyId === req.apiKeyId);
        if (key === undefined)
          throw new ConnectError("API key not found", Code.NotFound);
        key.state = ApiKeyState.REVOKED;
        key.revokeTime = ts();
        return { apiKey: key };
      },
    });

    router.service(SandboxService, {
      createSandbox(req) {
        const { org } = membership(req.organizationId);
        const existing = org.sandboxes.find(
          (s) => s.labels["mock-idempotency-key"] === req.idempotencyKey,
        );
        // A retry returns the same view as the first call: without the
        // fake's idempotency bookkeeping label.
        if (existing !== undefined) {
          return { sandbox: withoutMockLabels(existing) };
        }
        const device =
          org.devices.find((d) => d.deviceId === req.placement?.deviceId) ??
          org.devices.find((d) => d.state === DeviceState.ONLINE);
        if (device === undefined) {
          throw new ConnectError(
            "no device can run this sandbox",
            Code.FailedPrecondition,
          );
        }
        const sandbox = create(SandboxSchema, {
          sandboxId: uuidV7(),
          organizationId: req.organizationId,
          deviceId: device.deviceId,
          state: SandboxState.RUNNING,
          source: req.source,
          resources: req.resources,
          hostOs: device.hostOs,
          arch: device.arch,
          guestOs: GuestOs.LINUX,
          network: req.network ?? { mode: NetworkMode.OUTBOUND },
          labels: { ...req.labels, "mock-idempotency-key": req.idempotencyKey },
          idleTtl: req.idleTtl,
          maxTtl: req.maxTtl,
          idleDeadline: ts(Number(req.idleTtl?.seconds ?? 0n)),
          maxDeadline: ts(Number(req.maxTtl?.seconds ?? 0n)),
          createdBy: { kind: PrincipalKind.USER, id: userId },
          createTime: ts(),
        });
        // The fake hides its idempotency bookkeeping from the labels it returns.
        org.sandboxes.unshift(sandbox);
        return { sandbox: { ...sandbox, labels: { ...req.labels } } };
      },
      getSandbox(req) {
        const { org } = membership(req.organizationId);
        const s = findSandbox(org, req.sandboxId);
        return { sandbox: withoutMockLabels(s) };
      },
      listSandboxes(req) {
        const { org } = membership(req.organizationId);
        const ended = (s: Sandbox) =>
          s.state === SandboxState.DELETED || s.state === SandboxState.FAILED;
        const sandboxes = org.sandboxes
          .filter(
            (s) => req.includeEnded || req.state !== undefined || !ended(s),
          )
          .filter((s) => req.state === undefined || s.state === req.state)
          .filter(
            (s) => req.deviceId === undefined || s.deviceId === req.deviceId,
          )
          .map(withoutMockLabels);
        return { sandboxes };
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
        return { sandbox: withoutMockLabels(s) };
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
        return { sandbox: withoutMockLabels(s) };
      },
      resumeSandbox(req) {
        const { org } = membership(req.organizationId);
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
        return { sandbox: withoutMockLabels(s) };
      },
    });

    router.service(SnapshotService, {
      createSnapshot(req) {
        const { org } = membership(req.organizationId);
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
          snapshotId: uuidV7(),
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
        return {
          snapshots: org.snapshots.filter(
            (s) =>
              req.sandboxId === undefined ||
              s.sourceSandboxId === req.sandboxId,
          ),
        };
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
        const s = findSnapshot(org, req.snapshotId);
        s.exportState = SnapshotExportState.EXPORTED;
        return { snapshot: s };
      },
      createTemplate(req) {
        const { org } = membership(req.organizationId);
        findSnapshot(org, req.snapshotId);
        if (req.name.trim() === "")
          throw new ConnectError("name is required", Code.InvalidArgument);
        const template = create(TemplateSchema, {
          templateId: uuidV7(),
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
        return { templates: org.templates };
      },
      deleteTemplate(req) {
        const { org } = membership(req.organizationId);
        org.templates = org.templates.filter(
          (t) => t.templateId !== req.templateId,
        );
        return {};
      },
    });
  });
}

function withoutMockLabels(s: Sandbox): Sandbox {
  const { "mock-idempotency-key": _key, ...labels } = s.labels;
  return { ...s, labels };
}
