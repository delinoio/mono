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
import { type UtcMonth, utcMonthOf } from "../format.ts";
import { awaitDeferredCall } from "./form-submit-deferral.ts";

type Init<T> = Omit<T, "$typeName" | "$unknown">;

export const OWNER = "user-owner";
export const ADMIN = "user-admin";
export const VIEWER = "user-viewer";
export const OUTSIDER = "user-outsider";

export const ACCOUNT_ID = "0193a1b2-0000-7000-8000-000000000001";
export const INVITATION_TOKEN = "bai_valid-token";

const T0 = new Date("2026-11-01T00:00:00Z");
const ts = (date: Date = T0) => timestampFromDate(date);

export interface FakeAccount {
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
  /**
   * Fails the next call of a method that carries a `page_token` (a
   * continuation page, never the first page) with the given error once.
   */
  failContinuation: Map<string, ConnectError>;
  /**
   * Holds every call of a method until the promise settles, so that a test
   * can observe what the page does while a request is still in flight. Only
   * `GetBillingAccount`, `GetInvoiceUrl`, `ListBillingAccounts`,
   * `ListLedgerEntries`, `ListInvoices` and `ListUsageSummary` honor a gate. `AcceptInvitation` honors a gate
   * per invitation token, under the key `AcceptInvitation:<token>`, so that a
   * test can hold the request of one link while another link goes through.
   */
  gates: Map<string, Promise<unknown>>;
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

/** Assigns seeded usage to the given UTC month or the current month of each request. */
export function createFakeApi({
  usageMonth,
}: { usageMonth?: UtcMonth } = {}): FakeApi {
  const fixedUsageMonth =
    usageMonth === undefined ? undefined : { ...usageMonth };
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
    failContinuation: new Map<string, ConnectError>(),
    gates: new Map<string, Promise<unknown>>(),
  } as FakeApi;
  let nextId = 100;
  const newId = () =>
    `0193a1b2-0000-7000-8000-${String(nextId++).padStart(12, "0")}`;

  const record = (method: string, request: object) => {
    api.calls.push({ method, request: { ...request } });
    const continuation = api.failContinuation.get(method);
    if (
      continuation !== undefined &&
      "pageToken" in request &&
      request.pageToken !== ""
    ) {
      api.failContinuation.delete(method);
      throw continuation;
    }
    const failure = api.failNext.get(method);
    if (failure !== undefined) {
      api.failNext.delete(method);
      throw failure;
    }
  };
  /**
   * Serves one page of a list like the server: at most `pageSize` items and
   * a `next_page_token` that is empty on the last page. A token is `p<n>`,
   * the index of the page. ListBillingAccounts, ListMembers,
   * ListInvitations, ListLinkedOrganizations, ListLedgerEntries,
   * ListInvoices and ListUsageSummary page; every other list returns all of
   * its items.
   */
  const paginate = <T>(
    items: T[],
    req: { pageSize: number; pageToken: string },
  ): { page: T[]; nextPageToken: string } => {
    const size = req.pageSize > 0 ? Math.min(req.pageSize, 200) : 50;
    const index = req.pageToken === "" ? 0 : Number(req.pageToken.slice(1));
    if (!/^(|p\d+)$/.test(req.pageToken)) {
      throw new ConnectError("invalid page token", Code.InvalidArgument);
    }
    const start = index * size;
    return {
      page: items.slice(start, start + size),
      nextPageToken: start + size < items.length ? `p${index + 1}` : "",
    };
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
      async createBillingAccount(req) {
        record("CreateBillingAccount", req);
        await awaitDeferredCall(api, "CreateBillingAccount");
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
      async getBillingAccount(req) {
        record("GetBillingAccount", req);
        await api.gates.get("GetBillingAccount");
        return { billingAccount: view(load(req.billingAccountId).account) };
      },
      async listBillingAccounts(req) {
        record("ListBillingAccounts", req);
        await api.gates.get("ListBillingAccounts");
        const { page, nextPageToken } = paginate(
          [...api.accounts.values()]
            .filter((a) => !a.deleted && member(a) !== undefined)
            .map(view),
          req,
        );
        return { billingAccounts: page, nextPageToken };
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
      async createInvitation(req) {
        record("CreateInvitation", req);
        await awaitDeferredCall(api, "CreateInvitation");
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
        const { page, nextPageToken } = paginate(
          account.invitations.filter(
            (i) => i.state === InvitationState.PENDING,
          ),
          req,
        );
        return { invitations: page, nextPageToken };
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
      async acceptInvitation(req) {
        record("AcceptInvitation", req);
        await api.gates.get(`AcceptInvitation:${req.invitationToken}`);
        const invitation = api.invitationTokens.get(req.invitationToken);
        if (invitation === undefined) {
          throw new ConnectError("invitation not found", Code.NotFound);
        }
        const account = api.accounts.get(invitation.accountId)!;
        // Like the production service, reject before touching the membership
        // or consuming the token, so that the link stays a stable failure.
        if (account.deleted) {
          throw new ConnectError(
            "the billing account was deleted",
            Code.FailedPrecondition,
          );
        }
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
        const { page, nextPageToken } = paginate(
          load(req.billingAccountId).account.members,
          req,
        );
        return { members: page, nextPageToken };
      },
      updateMemberRole(req) {
        record("UpdateMemberRole", req);
        const { account } = load(req.billingAccountId, PAYMENT_ROLES);
        const target = account.members.find((m) => m.userId === req.userId);
        if (target === undefined) {
          throw new ConnectError("member not found", Code.NotFound);
        }
        // Like the production service, protection follows the current role,
        // so it also covers an owner made by transferOwnership.
        if (target.role === BillingAccountRole.OWNER) {
          throw new ConnectError(
            "the owner's role cannot be changed; transfer the ownership instead",
            Code.FailedPrecondition,
          );
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
        const target = account.members.find((m) => m.userId === req.userId);
        if (target === undefined) {
          throw new ConnectError("member not found", Code.NotFound);
        }
        if (target.role === BillingAccountRole.OWNER) {
          throw new ConnectError(
            "the owner cannot be removed; transfer the ownership first",
            Code.FailedPrecondition,
          );
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
        if (req.userId === self.userId) {
          throw new ConnectError(
            "user_id must name another member",
            Code.InvalidArgument,
          );
        }
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
      async listLedgerEntries(req) {
        record("ListLedgerEntries", req);
        await api.gates.get("ListLedgerEntries");
        const { page, nextPageToken } = paginate(
          load(req.billingAccountId).account.ledger,
          req,
        );
        return { ledgerEntries: page, nextPageToken };
      },
      getSubscription(req) {
        record("GetSubscription", req);
        return {
          subscription: subscription(load(req.billingAccountId).account),
        };
      },
      async startSubscription(req) {
        record("StartSubscription", req);
        await awaitDeferredCall(api, "StartSubscription");
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
      async createTopUp(req) {
        record("CreateTopUp", req);
        await awaitDeferredCall(api, "CreateTopUp");
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
      async listInvoices(req) {
        record("ListInvoices", req);
        await api.gates.get("ListInvoices");
        const { page, nextPageToken } = paginate(
          load(req.billingAccountId, PAYMENT_ROLES).account.invoices,
          req,
        );
        return { invoices: page, nextPageToken };
      },
      async getInvoiceUrl(req) {
        record("GetInvoiceUrl", req);
        load(req.billingAccountId, PAYMENT_ROLES);
        // Hold the authorized response so access can change before delivery.
        await api.gates.get("GetInvoiceUrl");
        return {
          invoiceUrl: `https://sandbox-invoice.paddle.test/${req.paddleTransactionId}.pdf`,
        };
      },
    });
    service(BillingUsageService, {
      listLinkedOrganizations(req) {
        record("ListLinkedOrganizations", req);
        const { page, nextPageToken } = paginate(
          load(req.billingAccountId).account.linked,
          req,
        );
        return { linkedOrganizations: page, nextPageToken };
      },
      async listUsageSummary(req) {
        record("ListUsageSummary", req);
        await api.gates.get("ListUsageSummary");
        const { account } = load(req.billingAccountId);
        const { year, month } = fixedUsageMonth ?? utcMonthOf(new Date());
        const rows =
          req.year === year && req.month === month ? account.usage : [];
        // The server reports the total of the whole month on every page,
        // so sum all rows before paging them.
        const totalAmountUsdMicros = rows.reduce(
          (sum, row) => sum + row.amountUsdMicros,
          0n,
        );
        const { page, nextPageToken } = paginate(rows, req);
        return { rows: page, totalAmountUsdMicros, nextPageToken };
      },
    });
  });
  return api;
}

/** The numbered id and name that the list helpers below generate. */
export const numbered = (n: number) => String(n).padStart(4, "0");

/**
 * Adds billing accounts that the signed-in user (`api.userId` when called)
 * owns, named `Account 0001` and so on, after the seeded account. They hold
 * no members, invitations or linked organizations beyond that owner.
 */
export function addBillingAccounts(api: FakeApi, count: number) {
  for (let n = 1; n <= count; n++) {
    const id = `0193a1b2-0001-7000-8000-${String(n).padStart(12, "0")}`;
    api.accounts.set(id, {
      ...seedAccount(),
      id,
      name: `Account ${numbered(n)}`,
      members: [
        {
          billingAccountId: id,
          userId: api.userId,
          role: BillingAccountRole.OWNER,
          displayName: "Olivia Owner",
          createTime: ts(),
        },
      ],
      invitations: [],
      linked: [],
    });
  }
}

/** Adds viewers named `Member 0001` and so on to the account. */
export function addMembers(account: FakeAccount, count: number) {
  for (let n = 1; n <= count; n++) {
    account.members.push({
      billingAccountId: account.id,
      userId: `user-${numbered(n)}`,
      role: BillingAccountRole.VIEWER,
      displayName: `Member ${numbered(n)}`,
      createTime: ts(),
    });
  }
}

/** Adds pending invitations for `invitee-0001@example.com` and so on. */
export function addInvitations(account: FakeAccount, count: number) {
  for (let n = 1; n <= count; n++) {
    account.invitations.push({
      invitationId: `0193a1b2-0002-7000-8000-${String(n).padStart(12, "0")}`,
      billingAccountId: account.id,
      email: `invitee-${numbered(n)}@example.com`,
      role: BillingAccountRole.VIEWER,
      state: InvitationState.PENDING,
      inviterUserId: OWNER,
      createTime: ts(),
      expireTime: ts(new Date("2026-11-08T00:00:00Z")),
    });
  }
}

/** Adds linked organizations named `<prefix> 0001` and so on. */
export function addLinkedOrganizations(
  account: FakeAccount,
  count: number,
  prefix = "Org",
) {
  for (let n = 1; n <= count; n++) {
    account.linked.push({
      service: ServiceId.OWNSAND,
      organizationId: `${prefix.toLowerCase()}-${numbered(n)}`,
      organizationName: `${prefix} ${numbered(n)}`,
      linkTime: ts(),
      status: create(OrganizationBillingStatusSchema, {
        service: ServiceId.OWNSAND,
        organizationId: `${prefix.toLowerCase()}-${numbered(n)}`,
        billingAccountId: account.id,
        status: BillingStatus.ALLOWED,
        reason: BillingStatusReason.NONE,
        sequence: 1n,
      }),
    });
  }
}

/**
 * Adds usage ledger entries `ledger-0001` and so on, newest first like the
 * seeded entries, after the seeded ones. Each entry takes 1 micro-USD.
 */
export function addLedgerEntries(account: FakeAccount, count: number) {
  for (let n = 1; n <= count; n++) {
    account.ledger.push({
      ledgerEntryId: `0193a1b2-0003-7000-8000-${String(n).padStart(12, "0")}`,
      billingAccountId: account.id,
      kind: LedgerEntryKind.USAGE,
      amountUsdMicros: -1n,
      balanceAfterUsdMicros: account.balance,
      createTime: ts(new Date("2026-11-02T00:00:00Z")),
      paddleTransactionId: "",
      creditGrantId: "",
      service: "ownsand",
      organizationId: `org-${numbered(n)}`,
      meter: "ownsand.relay_egress",
      windowStart: ts(new Date("2026-11-02T00:00:00Z")),
      quantity: 1n,
    });
  }
}

/** Adds invoices for the transactions `txn_inv0001` and so on. */
export function addInvoices(account: FakeAccount, count: number) {
  for (let n = 1; n <= count; n++) {
    account.invoices.push({
      paddleTransactionId: `txn_inv${numbered(n)}`,
      kind: LedgerEntryKind.MANUAL_TOP_UP,
      amountUsdMicros: 1_000_000n,
      createTime: ts(new Date("2026-10-15T00:00:00Z")),
    });
  }
}

/**
 * Adds usage summary rows for the organizations `usage-org-0001` and so on,
 * after the seeded rows. Each row bills 1 micro-USD.
 */
export function addUsageRows(account: FakeAccount, count: number) {
  for (let n = 1; n <= count; n++) {
    account.usage.push({
      service: ServiceId.OWNSAND,
      organizationId: `usage-org-${numbered(n)}`,
      meter: Meter.OWNSAND_RELAY_EGRESS,
      quantity: 1n,
      amountUsdMicros: 1n,
    });
  }
}
