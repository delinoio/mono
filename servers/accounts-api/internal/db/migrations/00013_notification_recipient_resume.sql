-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up

ALTER TABLE billing_notifications ADD COLUMN resume_after_user_id text;
