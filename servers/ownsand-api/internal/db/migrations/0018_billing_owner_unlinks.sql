-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up

-- Keep owner unlinks apart from cleanup-only rows that older workers consume.
CREATE TABLE billing_owner_unlinks (
    organization_id uuid PRIMARY KEY REFERENCES organization_billing ON DELETE CASCADE,
    unlink_id uuid NOT NULL,
    billing_account_id uuid NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_time timestamptz NOT NULL
);
CREATE INDEX billing_owner_unlinks_due_idx ON billing_owner_unlinks (next_attempt_time);
CREATE INDEX organization_billing_link_presence_idx ON organization_billing (organization_id) WHERE billing_account_id IS NOT NULL;

-- Delete the intent in the same transaction that removes its link. This
-- includes a transfer, a presence repair and an unlink by an older writer.
-- A later link to the same account cannot inherit the old intent.
-- +goose StatementBegin
CREATE FUNCTION clear_billing_owner_unlink() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM billing_owner_unlinks WHERE organization_id = NEW.organization_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER clear_billing_owner_unlink
    AFTER UPDATE OF billing_account_id ON organization_billing
    FOR EACH ROW WHEN (OLD.billing_account_id IS DISTINCT FROM NEW.billing_account_id)
    EXECUTE FUNCTION clear_billing_owner_unlink();
