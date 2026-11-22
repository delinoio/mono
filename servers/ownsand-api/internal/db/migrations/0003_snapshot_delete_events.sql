-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Snapshot deletion on devices: a control event names the device and the
-- snapshot whose local copy the lease holder tells the agent to delete.

-- +goose Up

ALTER TABLE control_events ADD COLUMN snapshot_id uuid;
