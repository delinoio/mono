// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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

// int32Flag is a flag.Value for an int32 option. The standard int flag parses
// into a 64-bit int, so a value such as 4294967297 would wrap to 1 when it is
// narrowed to the int32 request field. This type rejects values that do not
// fit instead, so the flag parser reports a usage error naming the flag.
type int32Flag int32

func (f *int32Flag) String() string {
	if f == nil {
		return "0"
	}
	return strconv.FormatInt(int64(*f), 10)
}

func (f *int32Flag) Set(s string) error {
	// Base 0 matches the standard int flag (0x, 0o and 0b prefixes).
	n, err := strconv.ParseInt(s, 0, 32)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return fmt.Errorf("value out of range: must be between %d and %d", math.MinInt32, math.MaxInt32)
		}
		return errors.New("parse error")
	}
	*f = int32Flag(n)
	return nil
}

// int32Var registers an int32 flag that rejects out-of-range values.
func int32Var(fs *flag.FlagSet, name string, def int32, usage string) *int32 {
	v := def
	fs.Var((*int32Flag)(&v), name, usage)
	return &v
}

type pageFlags struct {
	size  int32
	token string
}

func addPageFlags(fs *flag.FlagSet) *pageFlags {
	p := &pageFlags{}
	fs.Var((*int32Flag)(&p.size), "page-size", "page size (default 50, at most 200)")
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
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "d"), 10, 64)
		if err == nil {
			const day = 24 * time.Hour
			// Reject counts whose product overflows time.Duration instead of
			// letting it wrap to an unrelated, possibly negative, duration.
			if n > int64(math.MaxInt64/day) || n < int64(math.MinInt64/day) {
				return 0, usageErr("--%s: invalid duration %q (for example 30m, 2h or 7d)", flagName, s)
			}
			return time.Duration(n) * day, nil
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
	res, err := c.Organizations().List(ctx, &ownsandv1.ListOrganizationsRequest{PageSize: page.size, PageToken: page.token})
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
	res, err := a.client.Members().List(ctx, &ownsandv1.ListMembersRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token})
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
	res, err := a.client.Members().ListInvitations(ctx, &ownsandv1.ListInvitationsRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token})
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
	// Standard input can block without data, for example on a terminal, and a
	// blocked read cannot observe ctx: read it through a reader that returns
	// when an interrupt cancels ctx.
	var r io.Reader = newCtxReader(ctx, a.env.Stdin)
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
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: "interrupted"}
		}
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
		if ctx.Err() == context.Canceled && errors.Is(err, context.Canceled) {
			return &exitError{code: ExitInterrupted, msg: "interrupted"}
		}
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
	req := &ownsandv1.ListDevicesRequest{PageSize: page.size, PageToken: page.token}
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
	vcpu := int32Var(fs, "vcpu", -1, "vCPU limit (required)")
	memory := fs.Int64("memory", -1, "memory limit in MiB (required)")
	disk := fs.Int64("disk", -1, "disk limit in GiB (required)")
	overcommit := int32Var(fs, "vcpu-overcommit", -1, "vCPU overcommit percentage, 100 to 400 in steps of 25 (default: the current value)")
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
		VcpuCount: proto.Int32(*vcpu), MemoryMib: proto.Int64(*memory), DiskGib: proto.Int64(*disk),
		VcpuOvercommitPercent: proto.Int32(*overcommit),
	}
	// Only an absent flag falls back to the current value. An explicit
	// negative value is validated like any other so it is never replaced
	// silently by the stored percentage.
	overcommitGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "vcpu-overcommit" {
			overcommitGiven = true
		}
	})
	if !overcommitGiven {
		// Every value is required: keep the device's current overcommit.
		dev, err := a.client.Devices().Get(ctx, &ownsandv1.GetDeviceRequest{OrganizationId: org, DeviceId: rest[0]})
		if err != nil {
			return err
		}
		limits.VcpuOvercommitPercent = proto.Int32(dev.GetDevice().GetLimits().GetVcpuOvercommitPercent())
	}
	if _, err := validate.DeviceLimitsInput(limits); err != nil {
		return deviceLimitsUsageErr(err)
	}
	res, err := a.client.Devices().ReplaceLimits(ctx, &ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org, DeviceId: rest[0], Limits: limits})
	if err != nil {
		return err
	}
	return a.printMessage(res.GetDevice())
}

// deviceLimitFlags maps the limit field paths that validate reports to the
// flags of device set-limits that supply them.
var deviceLimitFlags = map[string]string{
	"limits.vcpu_count":              "--vcpu",
	"limits.memory_mib":              "--memory",
	"limits.disk_gib":                "--disk",
	"limits.vcpu_overcommit_percent": "--vcpu-overcommit",
}

// deviceLimitsUsageErr turns a limits validation failure into a usage error
// that names the offending option, so the user sees which flag to fix, and
// keeps the field path of the message.
func deviceLimitsUsageErr(err error) error {
	var verr *validate.Error
	if errors.As(err, &verr) {
		if flagName, ok := deviceLimitFlags[verr.Field]; ok {
			return usageErr("%s: %v", flagName, err)
		}
	}
	return usageErr("%v", err)
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
	res, err := a.client.Devices().ListEnrollmentTokens(ctx, &ownsandv1.ListEnrollmentTokensRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token})
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
	res, err := a.client.APIKeys().List(ctx, &ownsandv1.ListApiKeysRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token})
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
	req := &ownsandv1.ListSnapshotsRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token}
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
	res, err := a.client.Snapshots().ListTemplates(ctx, &ownsandv1.ListTemplatesRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token})
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
