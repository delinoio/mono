-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Keep the last recovery of each link so consumers can detect a recovery
-- that the current-status feed did not return.

-- +goose Up

-- Zero marks links with no recorded recovery. The previous release can
-- continue to insert links and write statuses with its existing column lists.
ALTER TABLE organization_links ADD COLUMN last_allowed_sequence bigint NOT NULL DEFAULT 0;
