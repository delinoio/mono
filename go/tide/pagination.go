// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"context"
	"iter"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// paginate returns an iterator over the items of every page of a list RPC.
// It sends req first with its own page_token and then with each
// next_page_token, until a page has no next token. The caller's request is
// never modified. An error ends the iteration after it is yielded.
func paginate[Req, Res any, PReq interface {
	*Req
	proto.Message
}, PRes interface {
	*Res
	proto.Message
}, Item any](ctx context.Context, call func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req PReq, items func(PRes) []Item) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) {
		var zero Item
		if req == nil {
			req = PReq(new(Req))
		}
		page := proto.Clone(req).(PReq)
		tokenField := page.ProtoReflect().Descriptor().Fields().ByName("page_token")
		seen := map[string]bool{}
		for {
			res, err := call(ctx, connect.NewRequest((*Req)(page)))
			if err != nil {
				yield(zero, err)
				return
			}
			msg := PRes(res.Msg)
			for _, it := range items(msg) {
				if !yield(it, nil) {
					return
				}
			}
			next := nextPageToken(msg)
			if next == "" {
				return
			}
			if seen[next] {
				yield(zero, errorf("the server returned the page token of an earlier page again"))
				return
			}
			seen[next] = true
			page.ProtoReflect().Set(tokenField, protoreflect.ValueOfString(next))
		}
	}
}

func nextPageToken(m proto.Message) string {
	fd := m.ProtoReflect().Descriptor().Fields().ByName("next_page_token")
	if fd == nil {
		return ""
	}
	return m.ProtoReflect().Get(fd).String()
}

// AllOrganizations iterates over every organization of ListOrganizations.
func (c *Client) AllOrganizations(ctx context.Context, req *tidev1.ListOrganizationsRequest) iter.Seq2[*tidev1.Organization, error] {
	return paginate(ctx, c.organizations.ListOrganizations, req, (*tidev1.ListOrganizationsResponse).GetOrganizations)
}

// AllInvitations iterates over every invitation of ListInvitations.
func (c *Client) AllInvitations(ctx context.Context, req *tidev1.ListInvitationsRequest) iter.Seq2[*tidev1.Invitation, error] {
	return paginate(ctx, c.members.ListInvitations, req, (*tidev1.ListInvitationsResponse).GetInvitations)
}

// AllMembers iterates over every member of ListMembers.
func (c *Client) AllMembers(ctx context.Context, req *tidev1.ListMembersRequest) iter.Seq2[*tidev1.Member, error] {
	return paginate(ctx, c.members.ListMembers, req, (*tidev1.ListMembersResponse).GetMembers)
}

// AllGitHubInstallations iterates over every installation of
// ListGitHubInstallations.
func (c *Client) AllGitHubInstallations(ctx context.Context, req *tidev1.ListGitHubInstallationsRequest) iter.Seq2[*tidev1.GitHubInstallation, error] {
	return paginate(ctx, c.gitHub.ListGitHubInstallations, req, (*tidev1.ListGitHubInstallationsResponse).GetInstallations)
}

// AllAgentIntegrations iterates over every integration of
// ListAgentIntegrations.
func (c *Client) AllAgentIntegrations(ctx context.Context, req *tidev1.ListAgentIntegrationsRequest) iter.Seq2[*tidev1.AgentIntegration, error] {
	return paginate(ctx, c.agentIntegrations.ListAgentIntegrations, req, (*tidev1.ListAgentIntegrationsResponse).GetIntegrations)
}

// AllRunnerProfiles iterates over every runner profile of
// ListRunnerProfiles.
func (c *Client) AllRunnerProfiles(ctx context.Context, req *tidev1.ListRunnerProfilesRequest) iter.Seq2[*tidev1.RunnerProfile, error] {
	return paginate(ctx, c.runnerProfiles.ListRunnerProfiles, req, (*tidev1.ListRunnerProfilesResponse).GetProfiles)
}

// AllRequests iterates over every request of ListRequests.
func (c *Client) AllRequests(ctx context.Context, req *tidev1.ListRequestsRequest) iter.Seq2[*tidev1.Request, error] {
	return paginate(ctx, c.work.ListRequests, req, (*tidev1.ListRequestsResponse).GetRequests)
}

// AllRunners iterates over every runner of ListRunners.
func (c *Client) AllRunners(ctx context.Context, req *tidev1.ListRunnersRequest) iter.Seq2[*tidev1.Runner, error] {
	return paginate(ctx, c.work.ListRunners, req, (*tidev1.ListRunnersResponse).GetRunners)
}

// AllAPIKeys iterates over every personal API key of ListApiKeys.
func (c *Client) AllAPIKeys(ctx context.Context, req *tidev1.ListApiKeysRequest) iter.Seq2[*tidev1.ApiKey, error] {
	return paginate(ctx, c.apiKeys.ListApiKeys, req, (*tidev1.ListApiKeysResponse).GetApiKeys)
}

// AllAuditEvents iterates over every audit event of ListAuditEvents.
func (c *Client) AllAuditEvents(ctx context.Context, req *tidev1.ListAuditEventsRequest) iter.Seq2[*tidev1.AuditEvent, error] {
	return paginate(ctx, c.audit.ListAuditEvents, req, (*tidev1.ListAuditEventsResponse).GetEvents)
}

// AllDevSessionProfiles iterates over every profile of
// ListDevSessionProfiles.
func (c *Client) AllDevSessionProfiles(ctx context.Context, req *tidev1.ListDevSessionProfilesRequest) iter.Seq2[*tidev1.DevSessionProfile, error] {
	return paginate(ctx, c.devSessions.ListDevSessionProfiles, req, (*tidev1.ListDevSessionProfilesResponse).GetProfiles)
}

// AllDevSessions iterates over every dev session of ListDevSessions.
func (c *Client) AllDevSessions(ctx context.Context, req *tidev1.ListDevSessionsRequest) iter.Seq2[*tidev1.DevSession, error] {
	return paginate(ctx, c.devSessions.ListDevSessions, req, (*tidev1.ListDevSessionsResponse).GetDevSessions)
}

// Collect gathers every item of seq, or returns the first error.
func Collect[T any](seq iter.Seq2[T, error]) ([]T, error) {
	var out []T
	for it, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, it)
	}
	return out, nil
}
