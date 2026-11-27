-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
CREATE TABLE github_surplus_checks (
    installation_id uuid NOT NULL REFERENCES github_installations ON DELETE CASCADE,
    -- Historical requests can outlive their profile. The worker discards
    -- checks whose profile no longer exists.
    runner_profile_id uuid NOT NULL,
    organization_id uuid NOT NULL,
    generation uuid NOT NULL,
    check_after timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, runner_profile_id)
);

CREATE INDEX github_surplus_checks_due_idx ON github_surplus_checks (check_after, generation);
