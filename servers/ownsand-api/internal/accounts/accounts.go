// SPDX-License-Identifier: FSL-1.1-ALv2

// Package accounts reads member display names from the delino-accounts API.
//
// The delino-accounts API does not define the RPC that returns the display
// profiles of a list of users yet (docs/servers-accounts-api-foundation.md
// records it as planned). Until it exists, the control plane uses
// Unavailable, and ListMembers returns members without display names, which
// is the fallback the control plane contract requires when that API is
// unavailable. Replace Unavailable with a Connect client of that RPC, using
// the control plane's Logto machine-to-machine credential, in the change
// that adds the RPC and its scope.
package accounts

import (
	"context"
	"errors"
)

// DisplayNameSource returns display names by Logto user ID. Missing users are
// absent from the map.
type DisplayNameSource interface {
	DisplayNames(ctx context.Context, userIDs []string) (map[string]string, error)
}

// ErrUnavailable means the source cannot answer now.
var ErrUnavailable = errors.New("delino-accounts display names are unavailable")

// Unavailable is the source used until the delino-accounts RPC exists.
type Unavailable struct{}

// DisplayNames implements DisplayNameSource.
func (Unavailable) DisplayNames(context.Context, []string) (map[string]string, error) {
	return nil, ErrUnavailable
}
