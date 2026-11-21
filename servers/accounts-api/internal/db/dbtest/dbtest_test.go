// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import "testing"

func TestChildURL(t *testing.T) {
	for _, tc := range []struct{ admin, want string }{
		{"postgres://u:p@127.0.0.1:5432/postgres", "postgres://u:p@127.0.0.1:5432/accounts_test_x?dbname=accounts_test_x"},
		{"postgres://u@h/postgres?sslmode=disable&dbname=postgres", "postgres://u@h/accounts_test_x?sslmode=disable&dbname=postgres&dbname=accounts_test_x"},
	} {
		got, err := childURL(tc.admin, "accounts_test_x")
		if err != nil {
			t.Fatalf("childURL(%q) error = %v", tc.admin, err)
		}
		if got != tc.want {
			t.Errorf("childURL(%q) = %q, want %q", tc.admin, got, tc.want)
		}
	}
	if _, err := childURL("postgres://u:secret@[::1", "x"); err == nil {
		t.Fatal("childURL accepted a malformed URL")
	}
}
