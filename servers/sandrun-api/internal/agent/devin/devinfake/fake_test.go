// SPDX-License-Identifier: FSL-1.1-ALv2

package devinfake

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDefaultQueuePageAfterMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sessionIDs []string
		delete     bool
		lastCursor string
	}{
		{
			name:       "claim the first session",
			sessionIDs: []string{"ses-1", "ses-2"},
			lastCursor: "page-2",
		},
		{
			name:       "delete the first two sessions",
			sessionIDs: []string{"ses-1", "ses-2", "ses-3"},
			delete:     true,
			lastCursor: "page-3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := New(t, "token", "outpost")
			for _, sid := range tc.sessionIDs {
				fake.AddSession(sid, false)
			}
			request := func(method, suffix, body string, response any) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, fake.Server.URL+"/opbeta/outposts/devins"+suffix, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+fake.Token)
				req.Header.Set("Content-Type", "application/json")
				res, err := fake.Server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Fatalf("%s %s: status = %d, want 200", method, suffix, res.StatusCode)
				}
				if response != nil {
					if err := json.NewDecoder(res.Body).Decode(response); err != nil {
						t.Fatal(err)
					}
				}
			}
			checkPage := func(cursor, wantID, wantCursor string, wantNext bool, wantTotal int) string {
				t.Helper()
				query := url.Values{"outpost": {fake.OutpostID}, "phase": {"pending"}, "cursor": {cursor}}
				var page struct {
					Items []struct {
						Metadata struct {
							ID string `json:"session_id"`
						}
					}
					Cursor  string
					HasNext bool `json:"has_next_page"`
					Total   int
				}
				request(http.MethodGet, "?"+query.Encode(), "", &page)
				if len(page.Items) != 1 || page.Items[0].Metadata.ID != wantID {
					t.Fatalf("page at %q has items %+v, want only %q", cursor, page.Items, wantID)
				}
				if page.Cursor != wantCursor || page.HasNext != wantNext || page.Total != wantTotal {
					t.Fatalf("page at %q has cursor %q, next %t and total %d, want %q, %t and %d", cursor, page.Cursor, page.HasNext, page.Total, wantCursor, wantNext, wantTotal)
				}
				return page.Cursor
			}
			cursor := checkPage("", tc.sessionIDs[0], "page-1", true, len(tc.sessionIDs))
			if tc.delete {
				fake.Forget("ses-1")
				fake.DeleteSession("ses-2")
			} else {
				request(http.MethodPost, "/ses-1/claim", `{"acceptor_id":"test-acceptor"}`, nil)
			}
			checkPage(cursor, tc.sessionIDs[len(tc.sessionIDs)-1], tc.lastCursor, false, 1)
		})
	}
}
