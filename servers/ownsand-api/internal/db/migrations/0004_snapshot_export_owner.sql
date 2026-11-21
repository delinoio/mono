-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Durable owner of a snapshot export. The export command ID doubles as the
-- attempt ID: it names the object prefix of the attempt's parts and the
-- result that may finish the attempt. The channel ID names the agent
-- channel that received the command; a channel ID is unique across
-- restarts, so a state report on a different channel proves that the
-- export lost its owner. All columns are NULL for a snapshot that was never
-- exported and for an export that started before this migration (its parts
-- use the legacy object keys, and an EXPORTING row without an owner counts
-- as orphaned).

ALTER TABLE snapshots ADD COLUMN export_command_id uuid;
ALTER TABLE snapshots ADD COLUMN export_channel_id uuid;
