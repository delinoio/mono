-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
ALTER TABLE ownsand_links
    ADD COLUMN removal_token UUID,
    ADD COLUMN removal_expires_at TIMESTAMPTZ,
    ADD CONSTRAINT ownsand_links_removal_lease CHECK (
        (removal_token IS NULL AND removal_expires_at IS NULL)
        OR (removal_token IS NOT NULL AND removal_expires_at IS NOT NULL
            AND removing AND organization_deletion_attempt_id IS NULL)
    );
