-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
CREATE INDEX audit_events_org_create_time_idx ON audit_events (organization_id, create_time DESC, audit_event_id DESC);
