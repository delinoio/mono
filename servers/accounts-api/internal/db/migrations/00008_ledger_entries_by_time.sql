-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Index of the ledger list by creation time. Owning contract:
-- docs/servers-accounts-api-foundation.md (Storage, Pagination). The change
-- only adds an index, writes no rows and is compatible with the previous
-- release, which keeps using ledger_entries_by_account.

-- +goose Up

-- ListLedgerEntries orders by (create_time DESC, ledger_entry_id DESC) within
-- one billing account. The existing index orders by ledger_entry_id only and
-- stays for the legacy ID-only page tokens that the previous release issued.
CREATE INDEX ledger_entries_by_account_time
    ON ledger_entries (billing_account_id, create_time DESC, ledger_entry_id DESC);
