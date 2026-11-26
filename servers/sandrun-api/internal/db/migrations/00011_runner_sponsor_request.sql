-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
-- Sponsor identity is independent of assignment. A retained runner can
-- outlive its sponsor request, so retention does not enforce a foreign key.
ALTER TABLE runners ADD COLUMN sponsor_request_id uuid;
