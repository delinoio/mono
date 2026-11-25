// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/tide"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// positional parses fs and requires exactly the named positional arguments.
func (a *app) positional(fs *flag.FlagSet, args []string, names ...string) ([]string, error) {
	rest, err := a.parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(rest) != len(names) {
		want := ""
		for _, n := range names {
			want += " <" + n + ">"
		}
		return nil, usageErr("usage: tide %s [flags]%s", fs.Name(), want)
	}
	return rest, nil
}

// int32Flag is a flag.Value for an int32 option that rejects values that
// do not fit instead of wrapping them.
type int32Flag struct {
	v   int32
	set bool
}

func (f *int32Flag) String() string { return strconv.FormatInt(int64(f.v), 10) }

func (f *int32Flag) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return fmt.Errorf("value out of range: must be between %d and %d", math.MinInt32, math.MaxInt32)
		}
		return errors.New("parse error")
	}
	f.v, f.set = int32(n), true
	return nil
}

// int64Flag is a flag.Value that records whether it was set.
type int64Flag struct {
	v   int64
	set bool
}

func (f *int64Flag) String() string { return strconv.FormatInt(f.v, 10) }

func (f *int64Flag) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("parse error")
	}
	f.v, f.set = n, true
	return nil
}

// stringFlag is a flag.Value that records whether it was set.
type stringFlag struct {
	v   string
	set bool
}

func (f *stringFlag) String() string     { return f.v }
func (f *stringFlag) Set(s string) error { f.v, f.set = s, true; return nil }

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// parseDuration parses Go duration syntax and whole days ("7d").
func parseDuration(flagName, s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil || n < 0 || n > math.MaxInt64/int64(24*time.Hour) {
			return 0, usageErr("--%s: invalid duration %q", flagName, s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, usageErr("--%s: invalid duration %q", flagName, s)
	}
	return d, nil
}

// readSecret reads a credential from the file of a --*-file flag or from
// standard input, and removes one trailing line break. Credentials never
// come from command-line arguments, which other local users can read.
func (a *app) readSecret(file, what string) (string, error) {
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		b, err = io.ReadAll(io.LimitReader(a.env.Stdin, 1<<20))
	}
	if err != nil {
		return "", fmt.Errorf("read the %s: %w", what, err)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if s == "" {
		return "", usageErr("the %s is empty: pass it on standard input or with a --*-file flag", what)
	}
	return s, nil
}

// Enum parsers. Each accepts the lowercase value name with hyphens.

func parseEnum[E ~int32](flagName, s string, values map[string]E) (E, error) {
	if v, ok := values[s]; ok {
		return v, nil
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sortStrings(names)
	return 0, usageErr("--%s must be one of %s", flagName, strings.Join(names, ", "))
}

var (
	roleValues = map[string]tidev1.Role{"admin": tidev1.Role_ROLE_ADMIN, "member": tidev1.Role_ROLE_MEMBER}
	osValues   = map[string]tidev1.GuestOs{
		"linux": tidev1.GuestOs_GUEST_OS_LINUX, "macos": tidev1.GuestOs_GUEST_OS_MACOS, "windows": tidev1.GuestOs_GUEST_OS_WINDOWS,
	}
	kindValues = map[string]tidev1.WorkloadKind{
		"github-job": tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, "agent-session": tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION,
		"dev-session": tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION,
	}
	priorityValues = map[string]tidev1.Priority{
		"high": tidev1.Priority_PRIORITY_HIGH, "normal": tidev1.Priority_PRIORITY_NORMAL, "low": tidev1.Priority_PRIORITY_LOW,
	}
	vendorValues = map[string]tidev1.AgentVendor{
		"claude-code": tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, "cursor": tidev1.AgentVendor_AGENT_VENDOR_CURSOR,
		"openai-codex": tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, "devin": tidev1.AgentVendor_AGENT_VENDOR_DEVIN,
	}
	requestStateValues = map[string]tidev1.RequestState{
		"queued": tidev1.RequestState_REQUEST_STATE_QUEUED, "starting": tidev1.RequestState_REQUEST_STATE_STARTING,
		"running": tidev1.RequestState_REQUEST_STATE_RUNNING, "ended": tidev1.RequestState_REQUEST_STATE_ENDED,
		"canceled": tidev1.RequestState_REQUEST_STATE_CANCELED, "expired": tidev1.RequestState_REQUEST_STATE_EXPIRED,
		"failed": tidev1.RequestState_REQUEST_STATE_FAILED,
	}
)

func sortStrings(s []string) { sort.Strings(s) }

func dur(d *durationpb.Duration) string {
	if d == nil {
		return ""
	}
	return d.AsDuration().String()
}

// Organizations.

func (a *app) orgList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	c, err := a.sdk()
	if err != nil {
		return err
	}
	orgs, err := tide.Collect(c.AllOrganizations(ctx, &tidev1.ListOrganizationsRequest{PageSize: 200}))
	if err != nil {
		return err
	}
	selected, _ := a.orgID()
	rows := make([][]string, 0, len(orgs))
	for _, o := range orgs {
		mark := ""
		if o.GetOrganizationId() == selected {
			mark = "*"
		}
		rows = append(rows, []string{mark, o.GetOrganizationId(), o.GetName(), enumName(o.GetCallerRole())})
	}
	return a.printList(&tidev1.ListOrganizationsResponse{Organizations: orgs}, []string{"", "id", "name", "role"}, rows)
}

func (a *app) orgCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "name")
	if err != nil {
		return err
	}
	c, err := a.sdk()
	if err != nil {
		return err
	}
	res, err := c.Organizations().CreateOrganization(ctx, connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetOrganization())
}

func (a *app) orgUse(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "organization-id")
	if err != nil {
		return err
	}
	// With Logto tokens, check that the organization exists for the user.
	// A personal API key cannot read organizations.
	if a.getenv(EnvAPIKey) == "" {
		c, err := a.sdk()
		if err != nil {
			return err
		}
		if _, err := c.Organizations().GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: rest[0]})); err != nil {
			return err
		}
	}
	s, err := a.loadSettings()
	if err != nil {
		s = settings{}
	}
	s.OrganizationID = rest[0]
	if err := a.saveSettings(s); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Commands now use the organization %s.\n", rest[0])
	return nil
}

func (a *app) orgDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "organization-id")
	if err != nil {
		return err
	}
	c, err := a.sdk()
	if err != nil {
		return err
	}
	if _, err := c.Organizations().DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: rest[0]})); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Deleted the organization %s.\n", rest[0])
	return nil
}

// Members.

func (a *app) memberList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	ms, err := tide.Collect(c.AllMembers(ctx, &tidev1.ListMembersRequest{OrganizationId: org, PageSize: 200}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(ms))
	for _, m := range ms {
		rows = append(rows, []string{m.GetUserId(), m.GetDisplayName(), enumName(m.GetRole()), ts(m.GetJoinTime())})
	}
	return a.printList(&tidev1.ListMembersResponse{Members: ms}, []string{"user", "name", "role", "joined"}, rows)
}

func (a *app) memberInvite(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	role := fs.String("role", "member", "role: admin or member")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	r, err := parseEnum("role", *role, roleValues)
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Members().CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: org, Role: r}))
	if err != nil {
		return err
	}
	return a.printSecret(res.Msg.GetInvitation(), "invitation_secret", res.Msg.GetInvitationSecret())
}

func (a *app) memberRevokeInvite(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "invitation-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Members().RevokeInvitation(ctx, connect.NewRequest(&tidev1.RevokeInvitationRequest{OrganizationId: org, InvitationId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetInvitation())
}

func (a *app) memberSetRole(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	role := fs.String("role", "", "role: admin or member")
	rest, err := a.positional(fs, args, "user-id")
	if err != nil {
		return err
	}
	r, err := parseEnum("role", *role, roleValues)
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Members().UpdateMemberRole(ctx, connect.NewRequest(&tidev1.UpdateMemberRoleRequest{OrganizationId: org, UserId: rest[0], Role: r}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetMember())
}

func (a *app) memberRemove(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "user-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	if _, err := c.Members().RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org, UserId: rest[0]})); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Removed %s.\n", rest[0])
	return nil
}

func (a *app) memberLeave(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	me, err := a.callerUserID(ctx)
	if err != nil {
		return err
	}
	if _, err := c.Members().RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org, UserId: me})); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Left the organization %s.\n", org)
	return nil
}

// OwnSand link.

func (a *app) ownsandLink(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	osOrg := fs.String("ownsand-org", "", "OwnSand organization ID")
	keyFile := fs.String("api-key-file", "", "file with the OwnSand organization API key (default: standard input)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	if *osOrg == "" {
		return usageErr("--ownsand-org is required")
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	key, err := a.readSecret(*keyFile, "OwnSand API key")
	if err != nil {
		return err
	}
	res, err := c.OwnSandLinks().SetOwnSandLink(ctx, connect.NewRequest(&tidev1.SetOwnSandLinkRequest{OrganizationId: org, OwnsandOrganizationId: *osOrg, OwnsandApiKey: key}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetLink())
}

func (a *app) ownsandShow(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.OwnSandLinks().GetOwnSandLink(ctx, connect.NewRequest(&tidev1.GetOwnSandLinkRequest{OrganizationId: org}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetLink())
}

func (a *app) ownsandUnlink(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	if _, err := c.OwnSandLinks().RemoveOwnSandLink(ctx, connect.NewRequest(&tidev1.RemoveOwnSandLinkRequest{OrganizationId: org})); err != nil {
		return err
	}
	fmt.Fprintln(a.env.Stderr, "Removed the OwnSand link.")
	return nil
}

// GitHub.

func (a *app) githubInstallURL(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.GitHub().CreateGitHubInstallUrl(ctx, connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: org}))
	if err != nil {
		return err
	}
	if a.fmt == FormatJSON {
		return a.printMessage(res.Msg)
	}
	if _, err := fmt.Fprintln(a.env.Stdout, res.Msg.GetUrl()); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Open the URL in a browser before %s; the tide console completes the link.\n", ts(res.Msg.GetExpireTime()))
	return nil
}

func (a *app) githubList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	items, err := tide.Collect(c.AllGitHubInstallations(ctx, &tidev1.ListGitHubInstallationsRequest{OrganizationId: org, PageSize: 200}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, i := range items {
		rows = append(rows, []string{i.GetInstallationId(), i.GetGithubAccountLogin(), enumName(i.GetState()), i.GetRunnerGroupName()})
	}
	return a.printList(&tidev1.ListGitHubInstallationsResponse{Installations: items}, []string{"id", "account", "state", "runner group"}, rows)
}

func (a *app) githubRunnerGroups(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "installation-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.GitHub().ListGitHubRunnerGroups(ctx, connect.NewRequest(&tidev1.ListGitHubRunnerGroupsRequest{OrganizationId: org, InstallationId: rest[0]}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(res.Msg.GetRunnerGroups()))
	for _, g := range res.Msg.GetRunnerGroups() {
		rows = append(rows, []string{strconv.FormatInt(g.GetGithubRunnerGroupId(), 10), g.GetName(), g.GetVisibility(), strconv.FormatBool(g.GetIsDefault())})
	}
	return a.printList(res.Msg, []string{"id", "name", "visibility", "default"}, rows)
}

func (a *app) githubSetRunnerGroup(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "installation-id", "github-runner-group-id")
	if err != nil {
		return err
	}
	group, err := strconv.ParseInt(rest[1], 10, 64)
	if err != nil {
		return usageErr("github-runner-group-id must be a number")
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.GitHub().UpdateGitHubInstallation(ctx, connect.NewRequest(&tidev1.UpdateGitHubInstallationRequest{OrganizationId: org, InstallationId: rest[0], GithubRunnerGroupId: group}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetInstallation())
}

func (a *app) githubUnlink(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "installation-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	if _, err := c.GitHub().UnlinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.UnlinkGitHubInstallationRequest{OrganizationId: org, InstallationId: rest[0]})); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Unlinked the installation %s.\n", rest[0])
	return nil
}

// Agent integrations.

type integrationFlags struct {
	vendor, name, poolName, outpostName stringFlag
	credentialsFile                     string
	credentialsStdin                    bool
}

func addIntegrationFlags(fs *flag.FlagSet, create bool) *integrationFlags {
	f := &integrationFlags{}
	if create {
		fs.Var(&f.vendor, "vendor", "vendor: claude-code, cursor, openai-codex or devin")
	}
	fs.Var(&f.name, "name", "integration name")
	fs.Var(&f.poolName, "pool-name", "Cursor self-hosted pool name")
	fs.Var(&f.outpostName, "outpost-name", "Devin outpost name")
	fs.StringVar(&f.credentialsFile, "credentials-file", "", "file with the vendor credentials as a JSON object (create: default standard input)")
	if !create {
		fs.BoolVar(&f.credentialsStdin, "credentials-stdin", false, "read new credentials as a JSON object from standard input")
	}
	return f
}

func (f *integrationFlags) validateVendorSettings(v tidev1.AgentVendor) error {
	if f.poolName.set && v != tidev1.AgentVendor_AGENT_VENDOR_CURSOR {
		return usageErr("--pool-name applies only to cursor")
	}
	if f.outpostName.set && v != tidev1.AgentVendor_AGENT_VENDOR_DEVIN {
		return usageErr("--outpost-name applies only to devin")
	}
	return nil
}

// integrationSettings returns the settings oneof value for the vendor.
func integrationSettings(v tidev1.AgentVendor, f *integrationFlags) any {
	switch v {
	case tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		return &tidev1.ClaudeCodeSettings{}
	case tidev1.AgentVendor_AGENT_VENDOR_CURSOR:
		return &tidev1.CursorSettings{PoolName: f.poolName.v}
	case tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		return &tidev1.OpenAiCodexSettings{}
	case tidev1.AgentVendor_AGENT_VENDOR_DEVIN:
		return &tidev1.DevinSettings{OutpostName: f.outpostName.v}
	}
	return nil
}

// readIntegrationCredentials reads the vendor credentials message as JSON
// with the protobuf field names, for example {"environment_key": "..."}.
func (a *app) readIntegrationCredentials(v tidev1.AgentVendor, file string) (any, error) {
	raw, err := a.readSecret(file, "integration credentials")
	if err != nil {
		return nil, err
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: false}
	parseErr := func(err error) error {
		// The JSON parser error can quote the input, which holds secrets.
		return usageErr("the integration credentials must be a JSON object with the credential fields of the vendor (for example {\"environment_key\": \"...\"})")
	}
	switch v {
	case tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		m := &tidev1.ClaudeCodeCredentials{}
		if err := opts.Unmarshal([]byte(raw), m); err != nil {
			return nil, parseErr(err)
		}
		return m, nil
	case tidev1.AgentVendor_AGENT_VENDOR_CURSOR:
		m := &tidev1.CursorCredentials{}
		if err := opts.Unmarshal([]byte(raw), m); err != nil {
			return nil, parseErr(err)
		}
		return m, nil
	case tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		m := &tidev1.OpenAiCodexCredentials{}
		if err := opts.Unmarshal([]byte(raw), m); err != nil {
			return nil, parseErr(err)
		}
		return m, nil
	case tidev1.AgentVendor_AGENT_VENDOR_DEVIN:
		m := &tidev1.DevinCredentials{}
		if err := opts.Unmarshal([]byte(raw), m); err != nil {
			return nil, parseErr(err)
		}
		return m, nil
	}
	return nil, usageErr("unknown vendor")
}

func (a *app) integrationCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	f := addIntegrationFlags(fs, true)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	vendor, err := parseEnum("vendor", f.vendor.v, vendorValues)
	if err != nil {
		return err
	}
	if err := f.validateVendorSettings(vendor); err != nil {
		return err
	}
	if f.name.v == "" {
		return usageErr("--name is required")
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	creds, err := a.readIntegrationCredentials(vendor, f.credentialsFile)
	if err != nil {
		return err
	}
	req := &tidev1.CreateAgentIntegrationRequest{OrganizationId: org, Vendor: vendor, Name: f.name.v}
	switch s := integrationSettings(vendor, f).(type) {
	case *tidev1.ClaudeCodeSettings:
		req.Settings = &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: s}
	case *tidev1.CursorSettings:
		req.Settings = &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: s}
	case *tidev1.OpenAiCodexSettings:
		req.Settings = &tidev1.CreateAgentIntegrationRequest_OpenaiCodex{OpenaiCodex: s}
	case *tidev1.DevinSettings:
		req.Settings = &tidev1.CreateAgentIntegrationRequest_Devin{Devin: s}
	}
	switch m := creds.(type) {
	case *tidev1.ClaudeCodeCredentials:
		req.Credentials = &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: m}
	case *tidev1.CursorCredentials:
		req.Credentials = &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: m}
	case *tidev1.OpenAiCodexCredentials:
		req.Credentials = &tidev1.CreateAgentIntegrationRequest_OpenaiCodexCredentials{OpenaiCodexCredentials: m}
	case *tidev1.DevinCredentials:
		req.Credentials = &tidev1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: m}
	}
	res, err := c.AgentIntegrations().CreateAgentIntegration(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetIntegration())
}

func (a *app) integrationList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	items, err := tide.Collect(c.AllAgentIntegrations(ctx, &tidev1.ListAgentIntegrationsRequest{OrganizationId: org, PageSize: 200}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, i := range items {
		set := 0
		for _, cs := range i.GetCredentials() {
			if cs.GetCredentialSet() {
				set++
			}
		}
		rows = append(rows, []string{i.GetAgentIntegrationId(), i.GetName(), enumName(i.GetVendor()), fmt.Sprintf("%d/%d", set, len(i.GetCredentials()))})
	}
	return a.printList(&tidev1.ListAgentIntegrationsResponse{Integrations: items}, []string{"id", "name", "vendor", "credentials set"}, rows)
}

func (a *app) integrationGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "integration-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.AgentIntegrations().GetAgentIntegration(ctx, connect.NewRequest(&tidev1.GetAgentIntegrationRequest{OrganizationId: org, AgentIntegrationId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetIntegration())
}

func (a *app) integrationUpdate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	f := addIntegrationFlags(fs, false)
	rest, err := a.positional(fs, args, "integration-id")
	if err != nil {
		return err
	}
	if f.credentialsFile != "" && f.credentialsStdin {
		return usageErr("pass only one of --credentials-file and --credentials-stdin")
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	cur, err := c.AgentIntegrations().GetAgentIntegration(ctx, connect.NewRequest(&tidev1.GetAgentIntegrationRequest{OrganizationId: org, AgentIntegrationId: rest[0]}))
	if err != nil {
		return err
	}
	vendor := cur.Msg.GetIntegration().GetVendor()
	if err := f.validateVendorSettings(vendor); err != nil {
		return err
	}
	req := &tidev1.UpdateAgentIntegrationRequest{OrganizationId: org, AgentIntegrationId: rest[0], UpdateMask: &fieldmaskpb.FieldMask{}}
	if f.name.set {
		req.Name = f.name.v
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, "name")
	}
	if f.poolName.set || f.outpostName.set {
		switch s := integrationSettings(vendor, f).(type) {
		case *tidev1.CursorSettings:
			req.Settings = &tidev1.UpdateAgentIntegrationRequest_Cursor{Cursor: s}
		case *tidev1.DevinSettings:
			req.Settings = &tidev1.UpdateAgentIntegrationRequest_Devin{Devin: s}
		default:
			return usageErr("--pool-name applies only to cursor and --outpost-name only to devin")
		}
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, "settings")
	}
	if f.credentialsFile != "" || f.credentialsStdin {
		creds, err := a.readIntegrationCredentials(vendor, f.credentialsFile)
		if err != nil {
			return err
		}
		switch m := creds.(type) {
		case *tidev1.ClaudeCodeCredentials:
			req.Credentials = &tidev1.UpdateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: m}
		case *tidev1.CursorCredentials:
			req.Credentials = &tidev1.UpdateAgentIntegrationRequest_CursorCredentials{CursorCredentials: m}
		case *tidev1.OpenAiCodexCredentials:
			req.Credentials = &tidev1.UpdateAgentIntegrationRequest_OpenaiCodexCredentials{OpenaiCodexCredentials: m}
		case *tidev1.DevinCredentials:
			req.Credentials = &tidev1.UpdateAgentIntegrationRequest_DevinCredentials{DevinCredentials: m}
		}
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, "credentials")
	}
	if len(req.UpdateMask.Paths) == 0 {
		return usageErr("nothing to update: pass --name, --pool-name, --outpost-name, --credentials-file or --credentials-stdin")
	}
	res, err := c.AgentIntegrations().UpdateAgentIntegration(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetIntegration())
}

func (a *app) integrationDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "integration-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	if _, err := c.AgentIntegrations().DeleteAgentIntegration(ctx, connect.NewRequest(&tidev1.DeleteAgentIntegrationRequest{OrganizationId: org, AgentIntegrationId: rest[0]})); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Deleted the integration %s.\n", rest[0])
	return nil
}

// Runner profiles.

type profileFlags struct {
	name, kind, integration, workspace, os, image, template stringFlag
	vcpu                                                    int32Flag
	memory, disk                                            int64Flag
	idleTTL, maxTTL, maxQueueWait, priority                 stringFlag
	warm                                                    int32Flag
}

func addProfileFlags(fs *flag.FlagSet, create bool) *profileFlags {
	f := &profileFlags{}
	fs.Var(&f.name, "name", "profile name (lowercase kebab-case)")
	if create {
		fs.Var(&f.kind, "kind", "workload kind: github-job, agent-session or dev-session")
		fs.Var(&f.integration, "integration", "agent integration ID (agent-session)")
		fs.Var(&f.os, "os", "guest OS: linux, macos or windows")
	}
	fs.Var(&f.workspace, "workspace-path", "absolute guest workspace path (dev-session)")
	fs.Var(&f.image, "image", "OwnSand base image: linux, macos or windows (default: the guest OS)")
	fs.Var(&f.template, "template", "OwnSand template ID instead of a base image")
	fs.Var(&f.vcpu, "vcpu", "vCPU count")
	fs.Var(&f.memory, "memory-mib", "memory in MiB")
	fs.Var(&f.disk, "disk-gib", "disk in GiB")
	fs.Var(&f.idleTTL, "idle-ttl", "idle TTL, for example 1h")
	fs.Var(&f.maxTTL, "max-ttl", "maximum TTL, for example 24h or 7d")
	fs.Var(&f.maxQueueWait, "max-queue-wait", "maximum queue wait, for example 30m")
	fs.Var(&f.priority, "priority", "priority: high, normal or low")
	fs.Var(&f.warm, "warm-count", "warm runners (0 to 10)")
	return f
}

// setsResources reports whether a resource flag is set.
func (f *profileFlags) setsResources() bool { return f.vcpu.set || f.memory.set || f.disk.set }

// apply copies the set flags into p and returns the field mask paths of
// UpdateRunnerProfile.
func (f *profileFlags) apply(p *tidev1.RunnerProfile) ([]string, error) {
	var paths []string
	if f.name.set {
		p.Name = f.name.v
		paths = append(paths, "name")
	}
	if f.workspace.set {
		p.WorkspacePath = f.workspace.v
		paths = append(paths, "workspace_path")
	}
	switch {
	case f.image.set && f.template.set:
		return nil, usageErr("pass only one of --image and --template")
	case f.image.set:
		os, err := parseEnum("image", f.image.v, osValues)
		if err != nil {
			return nil, err
		}
		p.Source = &tidev1.RunnerProfile_BaseImage{BaseImage: os}
		paths = append(paths, "source")
	case f.template.set:
		p.Source = &tidev1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: f.template.v}
		paths = append(paths, "source")
	}
	if f.setsResources() {
		if p.Resources == nil {
			p.Resources = &tidev1.Resources{}
		}
		var resourcePaths []string
		if f.vcpu.set {
			p.Resources.VcpuCount = f.vcpu.v
			resourcePaths = append(resourcePaths, "resources.vcpu_count")
		}
		if f.memory.set {
			p.Resources.MemoryMib = f.memory.v
			resourcePaths = append(resourcePaths, "resources.memory_mib")
		}
		if f.disk.set {
			p.Resources.DiskGib = f.disk.v
			resourcePaths = append(resourcePaths, "resources.disk_gib")
		}
		if len(resourcePaths) == 3 {
			paths = append(paths, "resources")
		} else {
			paths = append(paths, resourcePaths...)
		}
	}
	for _, d := range []struct {
		f     *stringFlag
		name  string
		path  string
		field **durationpb.Duration
	}{
		{&f.idleTTL, "idle-ttl", "idle_ttl", &p.IdleTtl},
		{&f.maxTTL, "max-ttl", "max_ttl", &p.MaxTtl},
		{&f.maxQueueWait, "max-queue-wait", "max_queue_wait", &p.MaxQueueWait},
	} {
		if !d.f.set {
			continue
		}
		v, err := parseDuration(d.name, d.f.v)
		if err != nil {
			return nil, err
		}
		*d.field = durationpb.New(v)
		paths = append(paths, d.path)
	}
	if f.priority.set {
		pr, err := parseEnum("priority", f.priority.v, priorityValues)
		if err != nil {
			return nil, err
		}
		p.Priority = pr
		paths = append(paths, "priority")
	}
	if f.warm.set {
		p.WarmCount = f.warm.v
		paths = append(paths, "warm_count")
	}
	return paths, nil
}

func (a *app) profileCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	f := addProfileFlags(fs, true)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	kind, err := parseEnum("kind", f.kind.v, kindValues)
	if err != nil {
		return err
	}
	osys, err := parseEnum("os", f.os.v, osValues)
	if err != nil {
		return err
	}
	p := &tidev1.RunnerProfile{WorkloadKind: kind, GuestOs: osys, AgentIntegrationId: f.integration.v, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: osys}}
	if _, err := f.apply(p); err != nil {
		return err
	}
	if p.GetName() == "" {
		return usageErr("--name is required")
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.RunnerProfiles().CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: org, Profile: p}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetProfile())
}

func (a *app) profileList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	kind := fs.String("kind", "", "only this workload kind: github-job, agent-session or dev-session")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	req := &tidev1.ListRunnerProfilesRequest{PageSize: 200}
	if *kind != "" {
		k, err := parseEnum("kind", *kind, kindValues)
		if err != nil {
			return err
		}
		req.WorkloadKind = k
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	req.OrganizationId = org
	items, err := tide.Collect(c.AllRunnerProfiles(ctx, req))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		r := p.GetResources()
		rows = append(rows, []string{p.GetRunnerProfileId(), p.GetName(), enumName(p.GetWorkloadKind()), enumName(p.GetGuestOs()),
			fmt.Sprintf("%d vCPU, %d MiB, %d GiB", r.GetVcpuCount(), r.GetMemoryMib(), r.GetDiskGib()), enumName(p.GetPriority()), strconv.Itoa(int(p.GetWarmCount()))})
	}
	return a.printList(&tidev1.ListRunnerProfilesResponse{Profiles: items}, []string{"id", "name", "kind", "os", "resources", "priority", "warm"}, rows)
}

func (a *app) profileGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "profile-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.RunnerProfiles().GetRunnerProfile(ctx, connect.NewRequest(&tidev1.GetRunnerProfileRequest{OrganizationId: org, RunnerProfileId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetProfile())
}

func (a *app) profileUpdate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	f := addProfileFlags(fs, false)
	rest, err := a.positional(fs, args, "profile-id")
	if err != nil {
		return err
	}
	p := &tidev1.RunnerProfile{}
	paths, err := f.apply(p)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return usageErr("nothing to update: pass at least one profile flag")
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.RunnerProfiles().UpdateRunnerProfile(ctx, connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{
		OrganizationId: org, RunnerProfileId: rest[0], Profile: p, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetProfile())
}

func (a *app) profileDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "profile-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	if _, err := c.RunnerProfiles().DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{OrganizationId: org, RunnerProfileId: rest[0]})); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Deleted the runner profile %s.\n", rest[0])
	return nil
}

// Requests and runners.

func requestRow(r *tidev1.Request) []string {
	return []string{r.GetRequestId(), enumName(r.GetKind()), enumName(r.GetState()), enumName(r.GetPriority()), r.GetRunnerProfileId(), ts(r.GetQueuedAt())}
}

var requestHeaders = []string{"id", "kind", "state", "priority", "profile", "queued"}

func (a *app) requestList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	kind := fs.String("kind", "", "only this workload kind")
	var states multiFlag
	fs.Var(&states, "state", "only this state (repeatable)")
	profile := fs.String("profile", "", "only this runner profile ID")
	ended := fs.Bool("include-ended", false, "include ended requests")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	req := &tidev1.ListRequestsRequest{PageSize: 200, RunnerProfileId: *profile, IncludeEnded: *ended}
	if *kind != "" {
		k, err := parseEnum("kind", *kind, kindValues)
		if err != nil {
			return err
		}
		req.Kind = k
	}
	for _, s := range states {
		st, err := parseEnum("state", s, requestStateValues)
		if err != nil {
			return err
		}
		req.States = append(req.States, st)
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	req.OrganizationId = org
	items, err := tide.Collect(c.AllRequests(ctx, req))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, r := range items {
		rows = append(rows, requestRow(r))
	}
	return a.printList(&tidev1.ListRequestsResponse{Requests: items}, requestHeaders, rows)
}

func (a *app) requestGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "request-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Work().GetRequest(ctx, connect.NewRequest(&tidev1.GetRequestRequest{OrganizationId: org, RequestId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetRequest())
}

func (a *app) requestCancel(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "request-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Work().CancelRequest(ctx, connect.NewRequest(&tidev1.CancelRequestRequest{OrganizationId: org, RequestId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetRequest())
}

func runnerRow(r *tidev1.Runner) []string {
	return []string{r.GetRunnerId(), enumName(r.GetKind()), enumName(r.GetState()), enumName(r.GetGuestOs()), r.GetRequestId(), r.GetOwnsandDeviceId()}
}

var runnerHeaders = []string{"id", "kind", "state", "os", "request", "device"}

func (a *app) runnerList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	profile := fs.String("profile", "", "only this runner profile ID")
	ended := fs.Bool("include-ended", false, "include ended runners")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	items, err := tide.Collect(c.AllRunners(ctx, &tidev1.ListRunnersRequest{OrganizationId: org, PageSize: 200, RunnerProfileId: *profile, IncludeEnded: *ended}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, r := range items {
		rows = append(rows, runnerRow(r))
	}
	return a.printList(&tidev1.ListRunnersResponse{Runners: items}, runnerHeaders, rows)
}

func (a *app) runnerGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "runner-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Work().GetRunner(ctx, connect.NewRequest(&tidev1.GetRunnerRequest{OrganizationId: org, RunnerId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetRunner())
}

func (a *app) runnerDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "runner-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Work().DeleteRunner(ctx, connect.NewRequest(&tidev1.DeleteRunnerRequest{OrganizationId: org, RunnerId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetRunner())
}

// status prints the queue, the runners by state and the device pool.
func (a *app) status(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	queued, err := tide.Collect(c.AllRequests(ctx, &tidev1.ListRequestsRequest{OrganizationId: org, PageSize: 200, States: []tidev1.RequestState{tidev1.RequestState_REQUEST_STATE_QUEUED}}))
	if err != nil {
		return err
	}
	runners, err := tide.Collect(c.AllRunners(ctx, &tidev1.ListRunnersRequest{OrganizationId: org, PageSize: 200}))
	if err != nil {
		return err
	}
	capRes, err := c.Work().GetCapacity(ctx, connect.NewRequest(&tidev1.GetCapacityRequest{OrganizationId: org}))
	if err != nil {
		return err
	}
	byState := map[string]int{}
	for _, r := range runners {
		byState[enumName(r.GetState())]++
	}
	if a.fmt == FormatJSON {
		q, err := protoList(queued)
		if err != nil {
			return err
		}
		d, err := protoList(capRes.Msg.GetDevices())
		if err != nil {
			return err
		}
		return writeJSON(a.env.Stdout, map[string]any{"queue": q, "runners_by_state": byState, "devices": d})
	}
	w := &stickyWriter{w: a.env.Stdout}
	fmt.Fprintf(w, "%s (%d queued)\n", a.bold("Queue"), len(queued))
	rows := make([][]string, 0, len(queued))
	for _, r := range queued {
		rows = append(rows, requestRow(r))
	}
	if len(rows) > 0 {
		if err := a.printTable(w, requestHeaders, rows); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "\n%s (%d)\n", a.bold("Runners"), len(runners))
	states := make([]string, 0, len(byState))
	for s := range byState {
		states = append(states, s)
	}
	sortStrings(states)
	for _, s := range states {
		fmt.Fprintf(w, "  %s: %d\n", s, byState[s])
	}
	fmt.Fprintf(w, "\n%s (%d)\n", a.bold("Devices"), len(capRes.Msg.GetDevices()))
	drows := make([][]string, 0, len(capRes.Msg.GetDevices()))
	for _, d := range capRes.Msg.GetDevices() {
		al := d.GetAllocation()
		drows = append(drows, []string{d.GetName(), enumName(d.GetState()), d.GetHostOs() + "/" + d.GetArch(),
			fmt.Sprintf("%d/%d", al.GetVcpuCount(), d.GetEffectiveVcpuLimit()),
			fmt.Sprintf("%d/%d", al.GetMemoryMib(), d.GetLimits().GetMemoryMib()),
			fmt.Sprintf("%d/%d", al.GetDiskGib(), d.GetLimits().GetDiskGib())})
	}
	if len(drows) > 0 {
		if err := a.printTable(w, []string{"device", "state", "platform", "vcpu", "memory mib", "disk gib"}, drows); err != nil {
			return err
		}
	}
	return w.err
}

// Personal API keys.

func (a *app) apiKeyCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	keyName := fs.String("name", "", "key name (1 to 64 characters)")
	expiresIn := fs.String("expires-in", "", "expiry after now, for example 30d (default: no expiry)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	if *keyName == "" {
		return usageErr("--name is required")
	}
	d, err := parseDuration("expires-in", *expiresIn)
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	req := &tidev1.CreateApiKeyRequest{OrganizationId: org, Name: *keyName}
	if d > 0 {
		req.ExpiresAt = timestamppb.New(time.Now().Add(d))
	}
	res, err := c.APIKeys().CreateApiKey(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	return a.printSecret(res.Msg.GetApiKey(), "secret", res.Msg.GetSecret())
}

func (a *app) apiKeyList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	items, err := tide.Collect(c.AllAPIKeys(ctx, &tidev1.ListApiKeysRequest{OrganizationId: org, PageSize: 200}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, k := range items {
		rows = append(rows, []string{k.GetApiKeyId(), k.GetName(), k.GetDisplayPrefix(), enumName(k.GetState()), ts(k.GetExpiresAt()), ts(k.GetLastUsedAt())})
	}
	return a.printList(&tidev1.ListApiKeysResponse{ApiKeys: items}, []string{"id", "name", "prefix", "state", "expires", "last used"}, rows)
}

func (a *app) apiKeyRevoke(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "api-key-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.APIKeys().RevokeApiKey(ctx, connect.NewRequest(&tidev1.RevokeApiKeyRequest{OrganizationId: org, ApiKeyId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetApiKey())
}

// Organization settings.

func (a *app) orgSettingsGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Organizations().GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org}))
	if err != nil {
		return err
	}
	st := res.Msg.GetOrganization().GetSettings()
	if a.fmt == FormatJSON {
		return a.printMessage(st)
	}
	limit := "none"
	if st.MaxDevSessionsPerUser != nil {
		limit = strconv.Itoa(int(st.GetMaxDevSessionsPerUser()))
	}
	_, err = fmt.Fprintf(a.env.Stdout, "%s: %s\n", a.paint(ansiCyan, "max_dev_sessions_per_user"), limit)
	return err
}

func (a *app) updateDevSessionLimit(ctx context.Context, limit *int32) error {
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Organizations().UpdateOrganizationSettings(ctx, connect.NewRequest(&tidev1.UpdateOrganizationSettingsRequest{
		OrganizationId: org,
		Settings:       &tidev1.OrganizationSettings{MaxDevSessionsPerUser: limit},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"max_dev_sessions_per_user"}},
	}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetOrganization().GetSettings())
}

func (a *app) orgSettingsSetLimit(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "limit")
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(rest[0], 10, 32)
	if err != nil || n <= 0 {
		return usageErr("limit must be a positive integer")
	}
	v := int32(n)
	return a.updateDevSessionLimit(ctx, &v)
}

func (a *app) orgSettingsClearLimit(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	return a.updateDevSessionLimit(ctx, nil)
}

// protoList converts messages to generic JSON values.
func protoList[M proto.Message](items []M) ([]any, error) {
	out := make([]any, 0, len(items))
	for _, it := range items {
		b, err := jsonOpts.Marshal(it)
		if err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(bytes.Clone(b)))
	}
	return out, nil
}
