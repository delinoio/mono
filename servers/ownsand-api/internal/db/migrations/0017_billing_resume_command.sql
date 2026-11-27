-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up

-- Track only automatic resumes created after this migration.
ALTER TABLE sandboxes ADD COLUMN billing_resume_command_id uuid;
CREATE UNIQUE INDEX sandboxes_billing_resume_command_id_idx
    ON sandboxes (billing_resume_command_id) WHERE billing_resume_command_id IS NOT NULL;
