// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dbtesturl retargets Postgres fixture URLs to generated databases.
package dbtesturl

import (
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

var errInvalidURL = errors.New("invalid PostgreSQL test database URL")

// Child returns the admin URL retargeted to the generated database name.
// pgx lets the final dbname override the path and any earlier dbname or
// database setting. The error omits the URL and its credentials.
func Child(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err == nil {
		u.Path = "/" + name
		u.RawPath = ""
		u.RawQuery = appendDatabase(u.RawQuery, name)
		return u.String(), nil
	}

	scheme, rest, ok := strings.Cut(admin, "://")
	if !ok || (scheme != "postgres" && scheme != "postgresql") {
		return "", errInvalidURL
	}
	// Match pgx's userinfo boundary so an escaped slash in a password does
	// not count as an encoded socket host.
	hostAndPath := rest
	if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
		hostAndPath = rest[i+1:]
	}
	hostEnd := strings.IndexAny(hostAndPath, "/?")
	if hostEnd < 0 {
		hostEnd = len(hostAndPath)
	}
	if !strings.Contains(strings.ToLower(hostAndPath[:hostEnd]), "%2f") {
		return "", errInvalidURL
	}
	// net/url rejects escaped slashes in the authority. Validate with the
	// same parser as the admin connection before preserving the raw URL parts.
	if _, err := pgconn.ParseConfig(admin); err != nil {
		return "", errInvalidURL
	}
	authority := admin[:len(admin)-len(hostAndPath)+hostEnd]
	_, query, _ := strings.Cut(hostAndPath[hostEnd:], "?")
	return authority + "/" + name + "?" + appendDatabase(query, name), nil
}

func appendDatabase(query, name string) string {
	if query == "" {
		return "dbname=" + name
	}
	return query + "&dbname=" + name
}
