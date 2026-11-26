// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtesturl

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestChild(t *testing.T) {
	const name = "fixture_test_0011223344556677"
	cases := []struct {
		label     string
		authority string
		path      string
		query     string
		wantHost  string
		wantPort  uint16
		wantApp   string
	}{
		{
			label: "TCP", authority: "postgres://u:p@127.0.0.1:5432",
			path: "/postgres", wantHost: "127.0.0.1", wantPort: 5432,
		},
		{
			label: "TCP without a path", authority: "postgresql://fixture@localhost",
			wantHost: "localhost",
		},
		{
			label: "TCP with database aliases", authority: "postgresql://fixture@localhost",
			path: "/", query: "dbname=x&database=y&z=1&a=b%20c+d", wantHost: "localhost",
		},
		{
			label: "socket in query", authority: "postgresql://fixture@",
			path: "/postgres", query: "host=%2Fvar%2Frun%2Fpostgresql&sslmode=disable",
			wantHost: "/var/run/postgresql",
		},
		{
			label: "socket in authority", authority: "postgresql://fixture@%2Fvar%2Frun%2Fpostgresql",
			path: "/postgres", query: "database=other&dbname=shadow&application_name=socket%20fixture&sslmode=disable",
			wantHost: "/var/run/postgresql", wantApp: "socket fixture",
		},
		{
			label: "socket with credentials and port", authority: "postgres://fixture:secret%2Fvalue@%2frun%2fpostgresql:5433",
			wantHost: "/run/postgresql", wantPort: 5433,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			admin := tc.authority + tc.path
			want := tc.authority + "/" + name + "?"
			if tc.query != "" {
				admin += "?" + tc.query
				want += tc.query + "&"
			}
			want += "dbname=" + name

			got, err := Child(admin, name)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("Child() = %q, want %q", got, want)
			}
			cfg, err := pgconn.ParseConfig(got)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Database != name || cfg.Host != tc.wantHost {
				t.Fatalf("pgx selects host %q and database %q, want host %q and database %q", cfg.Host, cfg.Database, tc.wantHost, name)
			}
			if tc.wantPort != 0 && cfg.Port != tc.wantPort {
				t.Errorf("pgx selects port %d, want %d", cfg.Port, tc.wantPort)
			}
			if tc.wantApp != "" && cfg.RuntimeParams["application_name"] != tc.wantApp {
				t.Errorf("pgx selects application_name %q, want %q", cfg.RuntimeParams["application_name"], tc.wantApp)
			}
		})
	}
}

func TestChildErrorOmitsCredentials(t *testing.T) {
	for _, admin := range []string{
		"postgresql://fixture:secret-pw@[::1",
		"postgresql://fixture:secret-pw@local host:5432/%zz",
		"postgresql://fixture:secret-pw@%2Fvar%ZZrun/postgres",
		"postgresql://fixture:secret-pw@%2Fvar%2Frun/postgres?application_name=%ZZ",
	} {
		if _, err := Child(admin, "generated"); err == nil {
			t.Error("Child accepted a malformed URL")
		} else if strings.Contains(err.Error(), "secret-pw") || strings.Contains(err.Error(), "postgresql://") {
			t.Error("Child reported connection credentials or the admin URL")
		}
	}
}
