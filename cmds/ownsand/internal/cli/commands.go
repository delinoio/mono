// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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
		return nil, usageErr("usage: ownsand %s [flags]%s", fs.Name(), want)
	}
	return rest, nil
}

// idArg checks a positional UUID v7 argument.
func idArg(field, v string) error {
	if err := validate.UUIDv7(field, v); err != nil {
		return usageErr("%s: %v", field, err)
	}
	return nil
}

type pageFlags struct {
	size  int
	token string
}

func addPageFlags(fs *flag.FlagSet) *pageFlags {
	p := &pageFlags{}
	fs.IntVar(&p.size, "page-size", 0, "page size (default 50, at most 200)")
	fs.StringVar(&p.token, "page-token", "", "page token of a previous list call")
	return p
}

// orgSetup prepares a command that needs an organization.
func (a *app) orgSetup() (string, error) {
	org, err := a.requireOrg()
	if err != nil {
		return "", err
	}
	if _, err := a.sdk(); err != nil {
		return "", err
	}
	return org, nil
}

// Enum flag parsers. Each accepts the lowercase value name.

func parseRole(s string) (ownsandv1.Role, error) {
	switch s {
	case "admin":
		return ownsandv1.Role_ROLE_ADMIN, nil
	case "member":
		return ownsandv1.Role_ROLE_MEMBER, nil
	}
	return 0, usageErr("--role must be admin or member")
}

func parseHostOS(s string) (ownsandv1.HostOs, error) {
	switch s {
	case "linux":
		return ownsandv1.HostOs_HOST_OS_LINUX, nil
	case "macos":
		return ownsandv1.HostOs_HOST_OS_MACOS, nil
	case "windows":
		return ownsandv1.HostOs_HOST_OS_WINDOWS, nil
	}
	return 0, usageErr("--os must be linux, macos or windows")
}

func parseImage(s string) (ownsandv1.BaseImage, error) {
	switch s {
	case "linux":
		return ownsandv1.BaseImage_BASE_IMAGE_LINUX, nil
	case "macos":
		return ownsandv1.BaseImage_BASE_IMAGE_MACOS, nil
	case "windows":
		return ownsandv1.BaseImage_BASE_IMAGE_WINDOWS, nil
	}
	return 0, usageErr("--image must be linux, macos or windows")
}

func parseArch(s string) (ownsandv1.Arch, error) {
	switch s {
	case "amd64":
		return ownsandv1.Arch_ARCH_AMD64, nil
	case "arm64":
		return ownsandv1.Arch_ARCH_ARM64, nil
	}
	return 0, usageErr("--arch must be amd64 or arm64")
}

func parseNetwork(s string) (ownsandv1.NetworkMode, error) {
	switch s {
	case "outbound":
		return ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, nil
	case "none":
		return ownsandv1.NetworkMode_NETWORK_MODE_NONE, nil
	}
	return 0, usageErr("--network must be outbound or none")
}

func parseDeviceState(s string) (ownsandv1.DeviceState, error) {
	switch s {
	case "online":
		return ownsandv1.DeviceState_DEVICE_STATE_ONLINE, nil
	case "offline":
		return ownsandv1.DeviceState_DEVICE_STATE_OFFLINE, nil
	case "removed":
		return ownsandv1.DeviceState_DEVICE_STATE_REMOVED, nil
	}
	return 0, usageErr("--state must be online, offline or removed")
}

func parseSandboxState(s string) (ownsandv1.SandboxState, error) {
	v, ok := ownsandv1.SandboxState_value["SANDBOX_STATE_"+strings.ToUpper(s)]
	if !ok || v == 0 {
		return 0, usageErr("--state must be creating, running, paused, deleting, deleted, failed or unreachable")
	}
	return ownsandv1.SandboxState(v), nil
}

func parsePolicyMode(s string) (ownsandv1.AgentVersionPolicyMode, error) {
	switch s {
	case "follow-default":
		return ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT, nil
	case "pin":
		return ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, nil
	case "hold":
		return ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD, nil
	}
	return 0, usageErr("--mode must be follow-default, pin or hold")
}

// parseDuration accepts Go durations and a whole-day suffix (7d).
func parseDuration(flagName, s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, usageErr("--%s: invalid duration %q (for example 30m, 2h or 7d)", flagName, s)
	}
	return d, nil
}

// ---- org ----

func (a *app) orgList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	c, err := a.sdk()
	if err != nil {
		return err
	}
	res, err := c.Organizations().List(ctx, &ownsandv1.ListOrganizationsRequest{PageSize: int32(page.size), PageToken: page.token})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, o := range res.GetOrganizations() {
		rows = append(rows, []string{o.GetOrganizationId(), o.GetName(), enumName(o.GetCallerRole()), ts(o.GetCreateTime())})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "NAME", "ROLE", "CREATED"}, rows)
}

func (a *app) orgCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	orgName := fs.String("name", "", "organization name (required)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	if *orgName == "" {
		return usageErr("--name is required")
	}
	c, err := a.sdk()
	if err != nil {
		return err
	}
	res, err := c.Organizations().Create(ctx, &ownsandv1.CreateOrganizationRequest{Name: *orgName})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetOrganization())
}

func (a *app) orgGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Organizations().Get(ctx, &ownsandv1.GetOrganizationRequest{OrganizationId: org})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetOrganization())
}

func (a *app) orgDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Organizations().Delete(ctx, &ownsandv1.DeleteOrganizationRequest{OrganizationId: org})
	if err != nil {
		return err
	}
	return a.printDone(res, "Deleted organization "+org+".")
}

func (a *app) orgTransfer(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "user-id")
	if err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Organizations().TransferOwnership(ctx, &ownsandv1.TransferOwnershipRequest{OrganizationId: org, UserId: rest[0]})
	if err != nil {
		return err
	}
	return a.printDone(res, "Transferred ownership.")
}

// printDone prints an empty response: {} in JSON, a sentence in text.
func (a *app) printDone(res proto.Message, text string) error {
	if a.fmt == FormatJSON {
		return a.printMessage(res)
	}
	fmt.Fprintln(a.env.Stderr, text)
	return nil
}

// ---- member ----

func (a *app) memberList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Members().List(ctx, &ownsandv1.ListMembersRequest{OrganizationId: org, PageSize: int32(page.size), PageToken: page.token})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, m := range res.GetMembers() {
		rows = append(rows, []string{m.GetUserId(), enumName(m.GetRole()), m.GetDisplayName(), ts(m.GetJoinTime())})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"USER", "ROLE", "NAME", "JOINED"}, rows)
}

func (a *app) memberInvite(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	role := fs.String("role", "member", "role: admin or member")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	r, err := parseRole(*role)
	if err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Members().CreateInvitation(ctx, &ownsandv1.CreateInvitationRequest{OrganizationId: org, Role: r})
	if err != nil {
		return err
	}
	return a.printSecret(res.GetInvitation(), "invitation_secret", res.GetInvitationSecret())
}

func (a *app) memberInvitations(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Members().ListInvitations(ctx, &ownsandv1.ListInvitationsRequest{OrganizationId: org, PageSize: int32(page.size), PageToken: page.token})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, i := range res.GetInvitations() {
		rows = append(rows, []string{i.GetInvitationId(), enumName(i.GetRole()), enumName(i.GetState()), i.GetSecretPrefix(), ts(i.GetExpireTime())})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "ROLE", "STATE", "PREFIX", "EXPIRES"}, rows)
}

func (a *app) memberRevokeInvitation(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "invitation-id")
	if err != nil {
		return err
	}
	if err := idArg("invitation-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Members().RevokeInvitation(ctx, &ownsandv1.RevokeInvitationRequest{OrganizationId: org, InvitationId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetInvitation())
}

// memberAccept reads the invitation secret from standard input or from
// --secret-file, never from an argument, because other local users can
// read process arguments.
func (a *app) memberAccept(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	secretFile := fs.String("secret-file", "", "file that holds the invitation secret (default: standard input)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	var r io.Reader = a.env.Stdin
	if *secretFile != "" {
		f, err := os.Open(*secretFile)
		if err != nil {
			return fmt.Errorf("open the secret file: %w", err)
		}
		defer f.Close()
		r = f
	}
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read the invitation secret: %w", err)
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return usageErr("no invitation secret on standard input or in --secret-file")
	}
	c, err := a.sdk()
	if err != nil {
		return err
	}
	res, err := c.Members().AcceptInvitation(ctx, &ownsandv1.AcceptInvitationRequest{InvitationSecret: secret})
	if err != nil {
		return err
	}
	return a.printMessage(res)
}

func (a *app) memberSetRole(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	role := fs.String("role", "", "role: admin or member (required)")
	rest, err := a.positional(fs, args, "user-id")
	if err != nil {
		return err
	}
	r, err := parseRole(*role)
	if err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Members().UpdateRole(ctx, &ownsandv1.UpdateMemberRoleRequest{OrganizationId: org, UserId: rest[0], Role: r})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetMember())
}

func (a *app) memberRemove(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "user-id")
	if err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Members().Remove(ctx, &ownsandv1.RemoveMemberRequest{OrganizationId: org, UserId: rest[0]})
	if err != nil {
		return err
	}
	return a.printDone(res, "Removed member "+rest[0]+".")
}

// ---- device ----

// allocatedVCPU returns the exact allocated vCPUs. A server that predates
// vcpu_total_count leaves it unset, and the int32 vcpu_count, which is capped
// at 2147483647 for a larger total, is then the best value.
func allocatedVCPU(a *ownsandv1.DeviceAllocation) int64 {
	if a != nil && a.VcpuTotalCount != nil {
		return a.GetVcpuTotalCount()
	}
	return int64(a.GetVcpuCount())
}

func deviceRows(ds []*ownsandv1.Device) [][]string {
	var rows [][]string
	for _, d := range ds {
		l := d.GetLimits()
		rows = append(rows, []string{
			d.GetDeviceId(), d.GetName(), enumName(d.GetState()), enumName(d.GetHostOs()) + "/" + enumName(d.GetArch()),
			fmt.Sprintf("%d/%d vCPU %d/%d MiB %d/%d GiB", allocatedVCPU(d.GetAllocation()), d.GetEffectiveVcpuLimitCount(), d.GetAllocation().GetMemoryMib(), l.GetMemoryMib(), d.GetAllocation().GetDiskGib(), l.GetDiskGib()),
			d.GetAgentVersion(),
		})
	}
	return rows
}

func (a *app) deviceList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	state := fs.String("state", "", "state filter: online, offline or removed")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	req := &ownsandv1.ListDevicesRequest{PageSize: int32(page.size), PageToken: page.token}
	if *state != "" {
		st, err := parseDeviceState(*state)
		if err != nil {
			return err
		}
		req.State = &st
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	req.OrganizationId = org
	res, err := a.client.Devices().List(ctx, req)
	if err != nil {
		return err
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "NAME", "STATE", "PLATFORM", "ALLOCATION", "AGENT"}, deviceRows(res.GetDevices()))
}

func (a *app) deviceGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "device-id")
	if err != nil {
		return err
	}
	if err := idArg("device-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().Get(ctx, &ownsandv1.GetDeviceRequest{OrganizationId: org, DeviceId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetDevice())
}

func (a *app) deviceSetLimits(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	vcpu := fs.Int("vcpu", -1, "vCPU limit (required)")
	memory := fs.Int64("memory", -1, "memory limit in MiB (required)")
	disk := fs.Int64("disk", -1, "disk limit in GiB (required)")
	overcommit := fs.Int("vcpu-overcommit", -1, "vCPU overcommit percentage, 100 to 400 in steps of 25 (default: the current value)")
	rest, err := a.positional(fs, args, "device-id")
	if err != nil {
		return err
	}
	if err := idArg("device-id", rest[0]); err != nil {
		return err
	}
	if *vcpu < 0 || *memory < 0 || *disk < 0 {
		return usageErr("--vcpu, --memory and --disk are required")
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	// ReplaceDeviceLimits needs every value to be present, so an explicit zero
	// (for example --vcpu 0) reaches the server as zero.
	limits := &ownsandv1.DeviceLimitsInput{
		VcpuCount: proto.Int32(int32(*vcpu)), MemoryMib: proto.Int64(*memory), DiskGib: proto.Int64(*disk),
		VcpuOvercommitPercent: proto.Int32(int32(*overcommit)),
	}
	if *overcommit < 0 {
		// Every value is required: keep the device's current overcommit.
		dev, err := a.client.Devices().Get(ctx, &ownsandv1.GetDeviceRequest{OrganizationId: org, DeviceId: rest[0]})
		if err != nil {
			return err
		}
		limits.VcpuOvercommitPercent = proto.Int32(dev.GetDevice().GetLimits().GetVcpuOvercommitPercent())
	}
	if _, err := validate.DeviceLimitsInput(limits); err != nil {
		return usageErr("%v", err)
	}
	res, err := a.client.Devices().ReplaceLimits(ctx, &ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org, DeviceId: rest[0], Limits: limits})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetDevice())
}

func (a *app) deviceRemove(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "device-id")
	if err != nil {
		return err
	}
	if err := idArg("device-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().Remove(ctx, &ownsandv1.RemoveDeviceRequest{OrganizationId: org, DeviceId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetDevice())
}

// ---- enrollment-token ----

func (a *app) enrollmentCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	ttl := fs.String("ttl", "1h", "token lifetime, at most 24h")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	d, err := parseDuration("ttl", *ttl)
	if err != nil {
		return err
	}
	if err := validate.EnrollmentTokenTTL(durationpb.New(d)); err != nil {
		return usageErr("%v", err)
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().CreateEnrollmentToken(ctx, &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org, Ttl: durationpb.New(d)})
	if err != nil {
		return err
	}
	return a.printSecret(res.GetToken(), "enrollment_token", res.GetEnrollmentToken())
}

func (a *app) enrollmentList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().ListEnrollmentTokens(ctx, &ownsandv1.ListEnrollmentTokensRequest{OrganizationId: org, PageSize: int32(page.size), PageToken: page.token})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, t := range res.GetTokens() {
		rows = append(rows, []string{t.GetEnrollmentTokenId(), enumName(t.GetState()), t.GetTokenPrefix(), ts(t.GetExpireTime()), t.GetDeviceId()})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "STATE", "PREFIX", "EXPIRES", "DEVICE"}, rows)
}

func (a *app) enrollmentRevoke(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "enrollment-token-id")
	if err != nil {
		return err
	}
	if err := idArg("enrollment-token-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().RevokeEnrollmentToken(ctx, &ownsandv1.RevokeEnrollmentTokenRequest{OrganizationId: org, EnrollmentTokenId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetToken())
}

// ---- api-key ----

func (a *app) apiKeyCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	keyName := fs.String("name", "", "API key name (required)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	if *keyName == "" {
		return usageErr("--name is required")
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.APIKeys().Create(ctx, &ownsandv1.CreateApiKeyRequest{OrganizationId: org, Name: *keyName})
	if err != nil {
		return err
	}
	return a.printSecret(res.GetApiKey(), "api_key_secret", res.GetApiKeySecret())
}

func (a *app) apiKeyList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.APIKeys().List(ctx, &ownsandv1.ListApiKeysRequest{OrganizationId: org, PageSize: int32(page.size), PageToken: page.token})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, k := range res.GetApiKeys() {
		rows = append(rows, []string{k.GetApiKeyId(), k.GetName(), enumName(k.GetState()), k.GetKeyPrefix(), ts(k.GetCreateTime())})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "NAME", "STATE", "PREFIX", "CREATED"}, rows)
}

func (a *app) apiKeyRevoke(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "api-key-id")
	if err != nil {
		return err
	}
	if err := idArg("api-key-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.APIKeys().Revoke(ctx, &ownsandv1.RevokeApiKeyRequest{OrganizationId: org, ApiKeyId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetApiKey())
}

// ---- agent-version ----

func (a *app) agentVersionGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().GetAgentVersionPolicy(ctx, &ownsandv1.GetAgentVersionPolicyRequest{OrganizationId: org})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetPolicy())
}

func (a *app) agentVersionSet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	mode := fs.String("mode", "", "follow-default, pin or hold (required)")
	version := fs.String("version", "", "pinned version (required for pin)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	m, err := parsePolicyMode(*mode)
	if err != nil {
		return err
	}
	if (m == ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN) != (*version != "") {
		return usageErr("--version is required with --mode pin and not allowed otherwise")
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Devices().UpdateAgentVersionPolicy(ctx, &ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org, Mode: m, PinnedVersion: *version})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetPolicy())
}

// ---- snapshot and template ----

func (a *app) snapshotCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "sandbox-id")
	if err != nil {
		return err
	}
	if err := idArg("sandbox-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().Create(ctx, &ownsandv1.CreateSnapshotRequest{OrganizationId: org, SandboxId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetSnapshot())
}

func (a *app) snapshotGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "snapshot-id")
	if err != nil {
		return err
	}
	if err := idArg("snapshot-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{OrganizationId: org, SnapshotId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetSnapshot())
}

func (a *app) snapshotList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	sandbox := fs.String("sandbox", "", "source sandbox filter")
	device := fs.String("device", "", "device filter")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	req := &ownsandv1.ListSnapshotsRequest{OrganizationId: org, PageSize: int32(page.size), PageToken: page.token}
	if *sandbox != "" {
		req.SandboxId = sandbox
	}
	if *device != "" {
		req.DeviceId = device
	}
	res, err := a.client.Snapshots().List(ctx, req)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, s := range res.GetSnapshots() {
		rows = append(rows, []string{s.GetSnapshotId(), enumName(s.GetState()), enumName(s.GetExportState()), s.GetSourceSandboxId(), s.GetDeviceId(), ts(s.GetCreateTime())})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "STATE", "EXPORT", "SANDBOX", "DEVICE", "CREATED"}, rows)
}

func (a *app) snapshotDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "snapshot-id")
	if err != nil {
		return err
	}
	if err := idArg("snapshot-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().Delete(ctx, &ownsandv1.DeleteSnapshotRequest{OrganizationId: org, SnapshotId: rest[0]})
	if err != nil {
		return err
	}
	return a.printDone(res, "Deleted snapshot "+rest[0]+".")
}

func (a *app) snapshotExport(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "snapshot-id")
	if err != nil {
		return err
	}
	if err := idArg("snapshot-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{OrganizationId: org, SnapshotId: rest[0]})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetSnapshot())
}

func (a *app) templateCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	snapshot := fs.String("snapshot", "", "source snapshot ID (required)")
	tname := fs.String("name", "", "template name (required)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	if *tname == "" {
		return usageErr("--name is required")
	}
	if err := idArg("--snapshot", *snapshot); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().CreateTemplate(ctx, &ownsandv1.CreateTemplateRequest{OrganizationId: org, SnapshotId: *snapshot, Name: *tname})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetTemplate())
}

func (a *app) templateList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().ListTemplates(ctx, &ownsandv1.ListTemplatesRequest{OrganizationId: org, PageSize: int32(page.size), PageToken: page.token})
	if err != nil {
		return err
	}
	var rows [][]string
	for _, t := range res.GetTemplates() {
		rows = append(rows, []string{t.GetTemplateId(), t.GetName(), t.GetSnapshotId(), ts(t.GetCreateTime())})
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "NAME", "SNAPSHOT", "CREATED"}, rows)
}

func (a *app) templateDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "template-id")
	if err != nil {
		return err
	}
	if err := idArg("template-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := a.client.Snapshots().DeleteTemplate(ctx, &ownsandv1.DeleteTemplateRequest{OrganizationId: org, TemplateId: rest[0]})
	if err != nil {
		return err
	}
	return a.printDone(res, "Deleted template "+rest[0]+".")
}
