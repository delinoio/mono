// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { Tabs, TabsList, TabsPanel, TabsTab } from "delino-ui/tabs";
import { useState } from "react";
import {
  BillingAccountProvider,
  Page,
  useBillingAccount,
} from "../components/shell.tsx";
import { ErrorNotice, Loading, PageHeader } from "../components/ui.tsx";
import { labels } from "../format.ts";
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

const TABS: { id: BillingTab; title: string; permission?: Permission }[] = [
  { id: BillingTab.Overview, title: "Overview" },
  { id: BillingTab.Usage, title: "Usage" },
  { id: BillingTab.Members, title: "Members" },
  { id: BillingTab.Organizations, title: "Organizations" },
  {
    id: BillingTab.Invoices,
    title: "Invoices",
    permission: Permission.ReadInvoices,
  },
  { id: BillingTab.Settings, title: "Settings" },
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
  const account = useQuery(BillingAccountService.method.getBillingAccount, {
    billingAccountId,
  });
  if (account.isPending) {
    return (
      <Page>
        <Loading label="Loading the billing account" />
      </Page>
    );
  }
  if (account.error) {
    return (
      <Page>
        <ErrorNotice
          error={account.error}
          onRetry={() => void account.refetch()}
        />
        <Link to={routePath({ id: RouteId.Billing })}>
          Go to your billing accounts
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
      }}
    >
      <Page>
        <BillingAccountBody />
      </Page>
    </BillingAccountProvider>
  );
}

function BillingAccountBody() {
  const { name, role } = useBillingAccount();
  const [tab, setTab] = useState<BillingTab>(BillingTab.Overview);
  const visible = TABS.filter(
    (t) => t.permission === undefined || can(role, t.permission),
  );
  return (
    <>
      <p className="breadcrumb">
        <Link to={routePath({ id: RouteId.Billing })}>Billing accounts</Link>
      </p>
      <PageHeader
        title={name}
        description={`Your role: ${labels.role[role]}`}
      />
      <Tabs value={tab} onValueChange={(value) => setTab(value as BillingTab)}>
        <TabsList aria-label="Billing account sections">
          {visible.map((t) => (
            <TabsTab key={t.id} value={t.id}>
              {t.title}
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
