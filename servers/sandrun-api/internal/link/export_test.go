// SPDX-License-Identifier: FSL-1.1-ALv2

package link

import "github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"

func (l *Links) CachedClientForTest(orgID string) ownsandclient.Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.clients[orgID].client
}
