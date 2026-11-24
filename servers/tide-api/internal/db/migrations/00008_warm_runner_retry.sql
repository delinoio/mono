-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
ALTER TABLE ownsand_links ADD COLUMN credential_revision uuid NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE runner_profiles
    ADD COLUMN warm_revision uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN warm_retry_link_revision uuid,
    ADD COLUMN warm_failures smallint NOT NULL DEFAULT 0,
    ADD COLUMN warm_create_rejections smallint NOT NULL DEFAULT 0,
    ADD COLUMN warm_retry_after timestamptz;

-- NULL marks a demand-driven runner or one created before this migration.
ALTER TABLE runners
    ADD COLUMN warm_profile_revision uuid,
    ADD COLUMN warm_link_revision uuid;
