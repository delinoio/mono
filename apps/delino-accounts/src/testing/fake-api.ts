// SPDX-License-Identifier: Apache-2.0
/**
 * An in-memory fake of the accounts-api user services for component tests.
 * It serves BillingAccountService, CreditService and BillingUsageService
 * through a Connect router transport, so pages run against real Connect
 * clients without a backend. It enforces the role table roughly like the
 * server so that tests can also check server-side errors.
 */
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import {
  type BillingAccountInvitation,
  type BillingAccountMember,
  BillingAccountRole,
  BillingAccountService,
  InvitationState,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  type AutoTopUpSettings,
  type CreditGrant,
  CreditService,
  type Invoice,
  type LedgerEntry,
  LedgerEntryKind,
  SubscriptionStatus,
} from "@delino/accounts-protos/v1/credit_pb";
import {
  BillingStatus,
  BillingStatusReason,
  BillingUsageService,
  type LinkedOrganization,
  Meter,
  OrganizationBillingStatusSchema,
  ServiceId,
  type UsageSummaryRow,
} from "@delino/accounts-protos/v1/usage_pb";

type Init<T> = Omit<T, "$typeName" | "$unknown">;

export const OWNER = "user-owner";
export const ADMIN = "user-admin";
export const VIEWER = "user-viewer";
export const OUTSIDER = "user-outsider";

export const ACCOUNT_ID = "0193a1b2-0000-7000-8000-000000000001";
export const INVITATION_TOKEN = "bai_valid-token";

const T0 = new Date("2026-11-01T00:00:00Z");
const ts = (date: Date = T0) => timestampFromDate(date);

interface FakeAccount {
  id: string;
  name: string;
  deleted: boolean;
  members: Init<BillingAccountMember>[];
  invitations: Init<BillingAccountInvitation>[];
  balance: bigint;
  grants: Init<CreditGrant>[];
  ledger: Init<LedgerEntry>[];
  subscription: {
    status: SubscriptionStatus;
    cancelAtPeriodEnd: boolean;
    periodEnd?: Date;
  };
  autoTopUp: Init<AutoTopUpSettings>;
  alertThreshold: bigint;
  linked: Init<LinkedOrganization>[];
  usage: Init<UsageSummaryRow>[];
  invoices: Init<Invoice>[];
}

/** A recorded request, for assertions. */
export interface Call {
  method: string;
  request: Record<string, unknown>;
}

export interface FakeApi {
  /** The signed-in user; change it to switch roles. */
  userId: string;
  accounts: Map<string, FakeAccount>;
  invitationTokens: Map<
    string,
    { accountId: string; role: BillingAccountRole }
  >;
  calls: Call[];
  /** Fails the next call of a method with the given error once. */
  failNext: Map<string, ConnectError>;
  transport: ReturnType<typeof createRouterTransport>;
}

function seedAccount(): FakeAccount {
  return {
    id: ACCOUNT_ID,
    name: "Acme Robotics",
    deleted: false,
    members: [
      {
        billingAccountId: ACCOUNT_ID,
        userId: OWNER,
        role: BillingAccountRole.OWNER,
        displayName: "Olivia Owner",
        createTime: ts(),
      },
      {
        billingAccountId: ACCOUNT_ID,
        userId: ADMIN,
        role: BillingAccountRole.ADMIN,
        displayName: "Adam Admin",
        createTime: ts(),
      },
      {
        billingAccountId: ACCOUNT_ID,
        userId: VIEWER,
        role: BillingAccountRole.VIEWER,
        displayName: "Vera Viewer",
        createTime: ts(),
      },
    ],
    invitations: [
      {
        invitationId: "0193a1b2-0000-7000-8000-0000000000a1",
        billingAccountId: ACCOUNT_ID,
        email: "pending@example.com",
        role: BillingAccountRole.VIEWER,
        state: InvitationState.PENDING,
        inviterUserId: OWNER,
        createTime: ts(),
        expireTime: ts(new Date("2026-11-08T00:00:00Z")),
      },
    ],
    balance: 12_345_678n,
    grants: [
      {
        creditGrantId: "0193a1b2-0000-7000-8000-0000000000g1",
        source: LedgerEntryKind.SUBSCRIPTION_CREDIT,
        originalUsdMicros: 10_000_000n,
        remainingUsdMicros: 2_345_678n,
        paddleTransactionId: "txn_sub1",
        createTime: ts(),
        expireTime: ts(new Date("2027-11-01T00:00:00Z")),
      },
      {
        creditGrantId: "0193a1b2-0000-7000-8000-0000000000g2",
        source: LedgerEntryKind.MANUAL_TOP_UP,
        originalUsdMicros: 25_000_000n,
        remainingUsdMicros: 10_000_000n,
        paddleTransactionId: "txn_top1",
        createTime: ts(),
        expireTime: ts(new Date("2027-11-02T00:00:00Z")),
      },
    ],
    ledger: [
      {
        ledgerEntryId: "0193a1b2-0000-7000-8000-0000000000l2",
        billingAccountId: ACCOUNT_ID,
        kind: LedgerEntryKind.USAGE,
        amountUsdMicros: -123n,
        balanceAfterUsdMicros: 12_345_678n,
        createTime: ts(new Date("2026-11-03T00:00:00Z")),
        paddleTransactionId: "",
        creditGrantId: "",
        service: "ownsand",
        organizationId: "org-1",
        meter: "ownsand.relay_egress",
        windowStart: ts(new Date("2026-11-03T00:00:00Z")),
        quantity: 1_048_576n,
      },
      {
        ledgerEntryId: "0193a1b2-0000-7000-8000-0000000000l1",
        billingAccountId: ACCOUNT_ID,
        kind: LedgerEntryKind.SUBSCRIPTION_CREDIT,
        amountUsdMicros: 10_000_000n,
        balanceAfterUsdMicros: 12_345_801n,
        createTime: ts(),
        paddleTransactionId: "txn_sub1",
        creditGrantId: "0193a1b2-0000-7000-8000-0000000000g1",
        service: "",
        organizationId: "",
        meter: "",
        quantity: 0n,
      },
    ],
    subscription: {
      status: SubscriptionStatus.ACTIVE,
      cancelAtPeriodEnd: false,
      periodEnd: new Date("2026-12-01T00:00:00Z"),
    },
    autoTopUp: {
      enabled: false,
      thresholdUsdMicros: 0n,
      amountUsdMicros: 0n,
      monthlyCapUsdMicros: 0n,
      consecutiveFailures: 0,
      monthToDateUsdMicros: 0n,
    },
    alertThreshold: 10_000_000n,
    linked: [
      {
        service: ServiceId.OWNSAND,
        organizationId: "org-1",
        organizationName: "Acme sandboxes",
        linkTime: ts(),
        status: create(OrganizationBillingStatusSchema, {
          service: ServiceId.OWNSAND,
          organizationId: "org-1",
          billingAccountId: ACCOUNT_ID,
          status: BillingStatus.ALLOWED,
          reason: BillingStatusReason.NONE,
          sequence: 1n,
        }),
      },
    ],
    usage: [
      {
        service: ServiceId.OWNSAND,
        organizationId: "org-1",
        meter: Meter.OWNSAND_RELAY_EGRESS,
        quantity: 1_048_576n,
        amountUsdMicros: 4_567n,
      },
      {
        service: ServiceId.OWNSAND,
        organizationId: "org-1",
        meter: Meter.OWNSAND_CLASS_A_RPC,
        quantity: 12_000n,
        amountUsdMicros: 1_250_000n,
      },
    ],
    invoices: [
      {
        paddleTransactionId: "txn_top1",
        kind: LedgerEntryKind.MANUAL_TOP_UP,
        amountUsdMicros: 25_000_000n,
        createTime: ts(),
      },
    ],
  };
}

const PAYMENT_ROLES = new Set([
  BillingAccountRole.OWNER,
  BillingAccountRole.ADMIN,
]);

export function createFakeApi(): FakeApi {
  const api = {
    userId: OWNER,
    accounts: new Map([[ACCOUNT_ID, seedAccount()]]),
    invitationTokens: new Map([
      [
        INVITATION_TOKEN,
        { accountId: ACCOUNT_ID, role: BillingAccountRole.VIEWER },
      ],
    ]),
    calls: [] as Call[],
    failNext: new Map<string, ConnectError>(),
  } as FakeApi;
  let nextId = 100;
  const newId = () =>
    `0193a1b2-0000-7000-8000-${String(nextId++).padStart(12, "0")}`;

  const record = (method: string, request: object) => {
    api.calls.push({ method, request: { ...request } });
    const failure = api.failNext.get(method);
    if (failure !== undefined) {
      api.failNext.delete(method);
      throw failure;
    }
  };
  const member = (account: FakeAccount) =>
    account.members.find((m) => m.userId === api.userId);
  const load = (id: string, roles?: Set<BillingAccountRole>) => {
    const account = api.accounts.get(id);
    const self = account && !account.deleted ? member(account) : undefined;
    if (account === undefined || self === undefined) {
      throw new ConnectError("billing account not found", Code.NotFound);
    }
    if (roles !== undefined && !roles.has(self.role)) {
      throw new ConnectError("permission denied", Code.PermissionDenied);
    }
    return { account, self };
  };
  const view = (account: FakeAccount) => ({
    billingAccountId: account.id,
    name: account.name,
    callerRole: member(account)?.role ?? BillingAccountRole.UNSPECIFIED,
    createTime: ts(),
  });
  const subscription = (account: FakeAccount) => ({
    status: account.subscription.status,
    cancelAtPeriodEnd: account.subscription.cancelAtPeriodEnd,
    currentPeriodEnd:
      account.subscription.periodEnd === undefined
        ? undefined
        : ts(account.subscription.periodEnd),
    priceUsdMicros: 10_000_000n,
  });

  api.transport = createRouterTransport(({ service }) => {
    service(BillingAccountService, {
      createBillingAccount(req) {
        record("CreateBillingAccount", req);
        const id = newId();
        const account = seedAccount();
        Object.assign(account, {
          id,
          name: req.name,
          members: [
            {
              billingAccountId: id,
              userId: api.userId,
              role: BillingAccountRole.OWNER,
              displayName: "",
              createTime: ts(),
            },
          ],
          invitations: [],
          balance: 0n,
          grants: [],
          ledger: [],
          subscription: {
            status: SubscriptionStatus.NONE,
            cancelAtPeriodEnd: false,
          },
          linked: [],
          usage: [],
          invoices: [],
        });
        api.accounts.set(id, account);
        return { billingAccount: view(account) };
      },
      getBillingAccount(req) {
        record("GetBillingAccount", req);
        return { billingAccount: view(load(req.billingAccountId).account) };
      },
      listBillingAccounts(req) {
        record("ListBillingAccounts", req);
        return {
          billingAccounts: [...api.accounts.values()]
            .filter((a) => !a.deleted && member(a) !== undefined)
            .map(view),
        };
      },
      updateBillingAccount(req) {
        record("UpdateBillingAccount", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        account.name = req.name;
        return { billingAccount: view(account) };
      },
      deleteBillingAccount(req) {
        record("DeleteBillingAccount", req);
        const { account } = load(
          req.billingAccountId,
          new Set([BillingAccountRole.OWNER]),
        );
        if (account.balance < 0n || account.linked.length > 0) {
          throw new ConnectError(
            "the billing account has a negative balance or linked organizations",
            Code.FailedPrecondition,
          );
        }
        account.deleted = true;
        return {};
      },
      createInvitation(req) {
        record("CreateInvitation", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        const invitation = {
          invitationId: newId(),
          billingAccountId: account.id,
          email: req.email,
          role: req.role,
          state: InvitationState.PENDING,
          inviterUserId: api.userId,
          createTime: ts(),
          expireTime: ts(new Date("2026-11-08T00:00:00Z")),
        };
        account.invitations.unshift(invitation);
        return { invitation };
      },
      listInvitations(req) {
        record("ListInvitations", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        return {
          invitations: account.invitations.filter(
            (i) => i.state === InvitationState.PENDING,
          ),
        };
      },
      revokeInvitation(req) {
        record("RevokeInvitation", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        const invitation = account.invitations.find(
          (i) => i.invitationId === req.invitationId,
        );
        if (invitation === undefined) {
          throw new ConnectError("invitation not found", Code.NotFound);
        }
        invitation.state = InvitationState.REVOKED;
        return { invitation };
      },
      acceptInvitation(req) {
        record("AcceptInvitation", req);
        const invitation = api.invitationTokens.get(req.invitationToken);
        if (invitation === undefined) {
          throw new ConnectError("invitation not found", Code.NotFound);
        }
        const account = api.accounts.get(invitation.accountId)!;
        if (member(account) !== undefined) {
          throw new ConnectError(
            "you are already a member of this billing account",
            Code.AlreadyExists,
          );
        }
        const joined = {
          billingAccountId: account.id,
          userId: api.userId,
          role: invitation.role,
          displayName: "",
          createTime: ts(),
        };
        account.members.push(joined);
        api.invitationTokens.delete(req.invitationToken);
        return { billingAccount: view(account), member: joined };
      },
      listMembers(req) {
        record("ListMembers", req);
        return { members: load(req.billingAccountId).account.members };
      },
      updateMemberRole(req) {
        record("UpdateMemberRole", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        const target = account.members.find((m) => m.userId === req.userId);
        if (target === undefined) {
          throw new ConnectError("member not found", Code.NotFound);
        }
        target.role = req.role;
        return { member: target };
      },
      removeMember(req) {
        record("RemoveMember", req);
        const { account, self } = load(req.billingAccountId);
        if (req.userId !== api.userId && !PAYMENT_ROLES.has(self.role)) {
          throw new ConnectError("permission denied", Code.PermissionDenied);
        }
        account.members = account.members.filter(
          (m) => m.userId !== req.userId,
        );
        return {};
      },
      transferOwnership(req) {
        record("TransferOwnership", req);
        const { account, self } = load(
          req.billingAccountId,
          new Set([BillingAccountRole.OWNER]),
        );
        const target = account.members.find((m) => m.userId === req.userId);
        if (target === undefined) {
          throw new ConnectError("member not found", Code.NotFound);
        }
        target.role = BillingAccountRole.OWNER;
        self.role = BillingAccountRole.ADMIN;
        return { billingAccount: view(account) };
      },
    });
    service(CreditService, {
      getBalance(req) {
        record("GetBalance", req);
        const { account } = load(req.billingAccountId);
        return {
          balanceUsdMicros: account.balance,
          creditGrants: account.grants,
        };
      },
      listLedgerEntries(req) {
        record("ListLedgerEntries", req);
        return { ledgerEntries: load(req.billingAccountId).account.ledger };
      },
      getSubscription(req) {
        record("GetSubscription", req);
        return {
          subscription: subscription(load(req.billingAccountId).account),
        };
      },
      startSubscription(req) {
        record("StartSubscription", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        account.subscription = {
          status: SubscriptionStatus.PENDING,
          cancelAtPeriodEnd: false,
        };
        return {
          paddleTransactionId: "txn_checkout1",
          subscription: subscription(account),
        };
      },
      cancelSubscription(req) {
        record("CancelSubscription", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        account.subscription.cancelAtPeriodEnd = true;
        return { subscription: subscription(account) };
      },
      resumeSubscription(req) {
        record("ResumeSubscription", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        account.subscription.cancelAtPeriodEnd = false;
        return { subscription: subscription(account) };
      },
      createTopUp(req) {
        record("CreateTopUp", req);
        load(req.billingAccountId, PAYMENT_ROLES);
        return {};
      },
      getAutoTopUpSettings(req) {
        record("GetAutoTopUpSettings", req);
        return { settings: load(req.billingAccountId).account.autoTopUp };
      },
      updateAutoTopUpSettings(req) {
        record("UpdateAutoTopUpSettings", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        const s = req.settings!;
        account.autoTopUp = {
          ...account.autoTopUp,
          enabled: s.enabled,
          thresholdUsdMicros: s.thresholdUsdMicros,
          amountUsdMicros: s.amountUsdMicros,
          monthlyCapUsdMicros: s.monthlyCapUsdMicros,
        };
        return { settings: account.autoTopUp };
      },
      getAlertSettings(req) {
        record("GetAlertSettings", req);
        return {
          settings: {
            lowBalanceThresholdUsdMicros: load(req.billingAccountId).account
              .alertThreshold,
          },
        };
      },
      updateAlertSettings(req) {
        record("UpdateAlertSettings", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        account.alertThreshold = req.settings!.lowBalanceThresholdUsdMicros;
        return {
          settings: { lowBalanceThresholdUsdMicros: account.alertThreshold },
        };
      },
      listInvoices(req) {
        record("ListInvoices", req);
        return {
          invoices: load(req.billingAccountId, PAYMENT_ROLES).account.invoices,
        };
      },
      getInvoiceUrl(req) {
        record("GetInvoiceUrl", req);
        load(req.billingAccountId, PAYMENT_ROLES);
        return {
          invoiceUrl: `https://sandbox-invoice.paddle.test/${req.paddleTransactionId}.pdf`,
        };
      },
    });
    service(BillingUsageService, {
      listLinkedOrganizations(req) {
        record("ListLinkedOrganizations", req);
        return {
          linkedOrganizations: load(req.billingAccountId).account.linked,
        };
      },
      listUsageSummary(req) {
        record("ListUsageSummary", req);
        const { account } = load(req.billingAccountId);
        // The seeded usage belongs to the current UTC month.
        const now = new Date();
        const rows =
          req.year === now.getUTCFullYear() &&
          req.month === now.getUTCMonth() + 1
            ? account.usage
            : [];
        return {
          rows,
          totalAmountUsdMicros: rows.reduce(
            (sum, row) => sum + row.amountUsdMicros,
            0n,
          ),
        };
      },
    });
  });
  return api;
}
