// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Tabs, TabsList, TabsPanel, TabsTab } from "delino-ui/tabs";
import { useState } from "react";
import {
  BillingAccountProvider,
  Page,
  useBillingAccount,
} from "../components/shell.tsx";
import {
  ErrorNotice,
  Loading,
  PageHeader,
  RefetchNotice,
} from "../components/ui.tsx";
import { labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import type { MessageKey } from "../text.ts";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { can, Permission } from "../roles.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import { InvoicesSection } from "./billing/invoices.tsx";
import { MembersSection } from "./billing/members.tsx";
import { OrganizationsSection } from "./billing/organizations.tsx";
import { OverviewSection } from "./billing/overview.tsx";
import { SettingsSection } from "./billing/settings.tsx";
import { UsageSection } from "./billing/usage.tsx";

/** Sections of a billing account page, in tab order. */
export enum BillingTab {
  Overview = "overview",
  Usage = "usage",
  Members = "members",
  Organizations = "organizations",
  Invoices = "invoices",
  Settings = "settings",
}

const TABS: { id: BillingTab; title: MessageKey; permission?: Permission }[] = [
  { id: BillingTab.Overview, title: "billingAccount.tab.overview" },
  { id: BillingTab.Usage, title: "billingAccount.tab.usage" },
  { id: BillingTab.Members, title: "billingAccount.tab.members" },
  { id: BillingTab.Organizations, title: "billingAccount.tab.organizations" },
  {
    id: BillingTab.Invoices,
    title: "billingAccount.tab.invoices",
    permission: Permission.ReadInvoices,
  },
  { id: BillingTab.Settings, title: "billingAccount.tab.settings" },
];

/**
 * Route `billing-account`: one billing account with its balance, credit,
 * subscription, usage, members, linked organizations, invoices and settings.
 */
export function BillingAccountPage({
  billingAccountId,
}: {
  billingAccountId: string;
}) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const { t } = useI18n();
  const accountQuery = createQueryOptions(
    BillingAccountService.method.getBillingAccount,
    { billingAccountId },
    { transport },
  );
  const account = useQuery({
    ...accountQuery,
    // Observe fetch completion even when data and its timestamp stay the same.
    notifyOnChangeProps: "all",
  });
  if (account.isPending) {
    return (
      <Page>
        <Loading label={t("billingAccount.loading")} />
      </Page>
    );
  }
  // A failed refetch of an account that has loaded keeps the page, its tab and
  // its dialogs, and shows a retryable notice above it. Only a first-load
  // failure, or a refetch that says the account is gone or no longer readable
  // for the caller, replaces the page.
  if (
    account.isLoadingError ||
    (account.isRefetchError && accountGone(account.error))
  ) {
    return (
      <Page>
        <ErrorNotice
          error={account.error}
          onRetry={() => void account.refetch()}
        />
        <Link to={routePath({ id: RouteId.Billing })}>
          {t("common.goToBillingAccounts")}
        </Link>
      </Page>
    );
  }
  const billingAccount = account.data.billingAccount!;
  return (
    <BillingAccountProvider
      value={{
        billingAccountId: billingAccount.billingAccountId,
        name: billingAccount.name,
        role: billingAccount.callerRole,
        queryKey: accountQuery.queryKey,
        readVersion:
          queryClient.getQueryState(accountQuery.queryKey)?.dataUpdateCount ??
          0,
      }}
    >
      <Page>
        <RefetchNotice
          error={account.isRefetchError ? account.error : undefined}
          onRetry={() => void account.refetch()}
        />
        <BillingAccountBody />
      </Page>
    </BillingAccountProvider>
  );
}

/**
 * Whether a failed refetch means that the caller cannot use the account any
 * more: it was deleted, or the caller was removed from it. Showing its stale
 * data would be wrong then. Any other failure is transient.
 */
function accountGone(error: unknown): boolean {
  return (
    error instanceof ConnectError &&
    (error.code === Code.NotFound || error.code === Code.PermissionDenied)
  );
}

function BillingAccountBody() {
  const { name, role } = useBillingAccount();
  const { t } = useI18n();
  const [tab, setTab] = useState<BillingTab>(BillingTab.Overview);
  const visible = TABS.filter(
    (item) => item.permission === undefined || can(role, item.permission),
  );
  // Persist the fallback so restoring access cannot reopen the hidden tab.
  if (!visible.some((item) => item.id === tab)) {
    setTab(BillingTab.Overview);
  }
  return (
    <>
      <p className="breadcrumb">
        <Link to={routePath({ id: RouteId.Billing })}>
          {t("billingAccounts.title")}
        </Link>
      </p>
      <PageHeader
        title={name}
        description={t("billingAccount.yourRole", {
          role: t(labels.role[role]),
        })}
      />
      <Tabs value={tab} onValueChange={(value) => setTab(value as BillingTab)}>
        <TabsList aria-label={t("billingAccount.sections")}>
          {visible.map((item) => (
            <TabsTab key={item.id} value={item.id}>
              {t(item.title)}
            </TabsTab>
          ))}
        </TabsList>
        <TabsPanel value={BillingTab.Overview}>
          <OverviewSection />
        </TabsPanel>
        <TabsPanel value={BillingTab.Usage}>
          <UsageSection />
        </TabsPanel>
        <TabsPanel value={BillingTab.Members}>
          <MembersSection />
        </TabsPanel>
        <TabsPanel value={BillingTab.Organizations}>
          <OrganizationsSection />
        </TabsPanel>
        {can(role, Permission.ReadInvoices) ? (
          <TabsPanel value={BillingTab.Invoices}>
            <InvoicesSection />
          </TabsPanel>
        ) : null}
        <TabsPanel value={BillingTab.Settings}>
          <SettingsSection />
        </TabsPanel>
      </Tabs>
    </>
  );
}
