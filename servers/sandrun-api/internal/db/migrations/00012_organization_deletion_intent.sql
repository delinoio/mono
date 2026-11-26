-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
ALTER TABLE organizations ADD COLUMN deletion_attempt_id UUID;

ALTER TABLE ownsand_links
    ADD COLUMN organization_deletion_attempt_id UUID,
    ADD CONSTRAINT ownsand_links_organization_deletion_requires_removal
        CHECK (organization_deletion_attempt_id IS NULL OR removing);
