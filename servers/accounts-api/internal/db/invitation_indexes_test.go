// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/db/dbtest"
)

// The migrations index the invitation volume limits of CreateInvitation by
// inviter and by address, each with the creation time.
func TestInvitationRateIndexes(t *testing.T) {
	d := dbtest.New(t)
	rows, err := d.Pool.Query(context.Background(), `SELECT indexname, indexdef FROM pg_indexes
		WHERE tablename = 'billing_account_invitations'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	defs := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		defs[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name, column := range map[string]string{
		"billing_account_invitations_by_inviter_time": "(inviter_user_id, create_time)",
		"billing_account_invitations_by_email_time":   "(email_key, create_time)",
	} {
		def, ok := defs[name]
		if !ok {
			t.Errorf("index %s is missing", name)
			continue
		}
		if !strings.Contains(def, column) || strings.Contains(def, "WHERE") {
			t.Errorf("index %s = %q, want the columns %s and no predicate", name, def, column)
		}
	}
}
