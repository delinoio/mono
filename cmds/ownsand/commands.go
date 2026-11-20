package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/proto"
)

const topUsage = `Usage: ownsand [global flags] <command> <subcommand> [flags] [arguments]

Commands:
  login                      sign in with the Logto device authorization flow
  logout                     delete the stored tokens
  org                        list, create, get, delete, transfer
  member                     list, invite, invitations, revoke-invitation, accept, set-role, remove
  device                     list, get, set-limits, remove
  enrollment-token           create, list, revoke
  api-key                    create, list, revoke
  agent-version              get, set
  sandbox                    create, get, list, delete, pause, resume, exec
  file                       read, write
  snapshot                   create, get, list, delete, export
  template                   create, list, delete

Global flags (also accepted after the subcommand):
  --org ID                   organization (default $OWNSAND_ORG)
  --format text|json         output format (default text)
  --api-origin URL           API origin (default $OWNSAND_API_ORIGIN or https://ownsand.api.delino.io)
  --no-color                 turn off ANSI color (also $NO_COLOR)
  --verbose                  write structured logs to standard error
  --transport auto|relay|peer-to-peer
                             force the data path transport (for tests)

Credentials: $OWNSAND_API_KEY when set, otherwise the tokens that "ownsand login" stored.
`

// command is one subcommand.
type command struct {
	usage string
	run   func(a *app, fs *flag.FlagSet, args []string) error
	// setup registers the command's own flags before parsing.
	flags func(fs *flag.FlagSet)
}

func (a *app) dispatch(args []string) error {
	switch args[0] {
	case "login":
		return a.runLeaf("login", args[1:], loginCommand())
	case "logout":
		return a.runLeaf("logout", args[1:], logoutCommand())
	case "help", "-h", "--help":
		fmt.Fprint(a.io.out, topUsage)
		return nil
	}
	group, ok := groups()[args[0]]
	if !ok {
		return usagef("unknown command %q; run \"ownsand help\"", args[0])
	}
	if len(args) < 2 {
		return usagef("%s needs a subcommand: %s", args[0], strings.Join(group.order, ", "))
	}
	cmd, ok := group.cmds[args[1]]
	if !ok {
		return usagef("unknown subcommand %q of %s: want one of %s", args[1], args[0], strings.Join(group.order, ", "))
	}
	return a.runLeaf(args[0]+" "+args[1], args[2:], cmd)
}

// runLeaf parses the flags of one command. Flags may come before and after
// positional arguments, except after "--".
func (a *app) runLeaf(name string, args []string, cmd command) error {
	fs := flag.NewFlagSet("ownsand "+name, flag.ContinueOnError)
	fs.SetOutput(a.io.err)
	a.g.register(fs)
	if cmd.flags != nil {
		cmd.flags(fs)
	}
	fs.Usage = func() {
		fmt.Fprintf(a.io.err, "Usage: ownsand %s %s\n\nFlags:\n", name, cmd.usage)
		fs.PrintDefaults()
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return &usageError{msg: err.Error()}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if len(args) > 0 && len(rest) < len(args) && args[len(args)-len(rest)-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	if err := a.setup(); err != nil {
		return err
	}
	return cmd.run(a, fs, pos)
}

type group struct {
	order []string
	cmds  map[string]command
}

func mkGroup(entries ...any) group {
	g := group{cmds: map[string]command{}}
	for i := 0; i < len(entries); i += 2 {
		name := entries[i].(string)
		g.order = append(g.order, name)
		g.cmds[name] = entries[i+1].(command)
	}
	return g
}

func needArgs(args []string, n int, names string) error {
	if len(args) != n {
		return usagef("want arguments: %s", names)
	}
	return nil
}

var listFlags = struct {
	pageSize  *int
	pageToken *string
}{}

func withList(extra func(fs *flag.FlagSet)) func(fs *flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		listFlags.pageSize = fs.Int("page-size", 0, "page size (server default 50, maximum 200)")
		listFlags.pageToken = fs.String("page-token", "", "page token from a previous list")
		if extra != nil {
			extra(fs)
		}
	}
}

func listOpts() ownsand.ListOptions {
	return ownsand.ListOptions{PageSize: int32(*listFlags.pageSize), PageToken: *listFlags.pageToken}
}

func msgs[T proto.Message](items []T) []proto.Message {
	out := make([]proto.Message, len(items))
	for i, m := range items {
		out[i] = m
	}
	return out
}

func parseRole(s string) (ownsandv1.Role, error) {
	switch s {
	case "admin":
		return ownsandv1.Role_ROLE_ADMIN, nil
	case "member":
		return ownsandv1.Role_ROLE_MEMBER, nil
	}
	return 0, usagef("role must be admin or member, got %q", s)
}

func groups() map[string]group {
	var (
		role, mode, version, name, image, snapshot, template, deviceID, hostOS, arch, network string
		idleTTL, maxTTL                                                                       string
		vcpu, memory, disk                                                                    int64
		secretFile, to, from, fileMode, expiresIn                                             string
		includeRemoved                                                                        bool
		labels                                                                                = kvFlag{}
	)
	return map[string]group{
		"org": mkGroup(
			"list", command{usage: "", flags: withList(nil), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, err := a.client()
				if err != nil {
					return err
				}
				items, next, err := c.ListOrganizations(a.ctx, listOpts())
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"create", command{usage: "<name>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<name>"); err != nil {
					return err
				}
				c, err := a.client()
				if err != nil {
					return err
				}
				o, err := c.CreateOrganization(a.ctx, args[0])
				if err != nil {
					return err
				}
				return a.printMessages(false, "", o)
			}},
			"get", command{run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				o, err := c.GetOrganization(a.ctx, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", o)
			}},
			"delete", command{run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.DeleteOrganization(a.ctx, opt...))
			}},
			"transfer", command{usage: "<new-owner-user-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<new-owner-user-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.TransferOwnership(a.ctx, args[0], opt...))
			}},
		),
		"member": mkGroup(
			"list", command{flags: withList(nil), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListMembers(a.ctx, listOpts(), opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"invite", command{usage: "--role admin|member", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&role, "role", "", "role of the invited member: admin or member (required)")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				r, err := parseRole(role)
				if err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				inv, secret, err := c.CreateInvitation(a.ctx, r, opt...)
				if err != nil {
					return err
				}
				return a.printSecret(secret, "invitation", inv)
			}},
			"invitations", command{flags: withList(nil), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListInvitations(a.ctx, listOpts(), opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"revoke-invitation", command{usage: "<invitation-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<invitation-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.RevokeInvitation(a.ctx, args[0], opt...))
			}},
			"accept", command{usage: "[--secret-file PATH] (the secret is read from standard input by default)", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&secretFile, "secret-file", "", "file that holds the invitation secret")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				if len(args) != 0 {
					return usagef("the invitation secret is not accepted as an argument; pass it on standard input or with --secret-file")
				}
				secret, err := a.readSecret(secretFile)
				if err != nil {
					return err
				}
				c, err := a.client()
				if err != nil {
					return err
				}
				org, m, err := c.AcceptInvitation(a.ctx, secret)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", org, m)
			}},
			"set-role", command{usage: "<user-id> admin|member", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 2, "<user-id> admin|member"); err != nil {
					return err
				}
				r, err := parseRole(args[1])
				if err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				m, err := c.UpdateMemberRole(a.ctx, args[0], r, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", m)
			}},
			"remove", command{usage: "<user-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<user-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.RemoveMember(a.ctx, args[0], opt...))
			}},
		),
		"device": mkGroup(
			"list", command{flags: withList(func(fs *flag.FlagSet) {
				fs.BoolVar(&includeRemoved, "include-removed", false, "include removed devices")
			}), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListDevices(a.ctx, listOpts(), includeRemoved, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"get", command{usage: "<device-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<device-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				d, err := c.GetDevice(a.ctx, args[0], opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", d)
			}},
			"set-limits", command{usage: "<device-id> --vcpu N --memory MiB --disk GiB", flags: func(fs *flag.FlagSet) {
				fs.Int64Var(&vcpu, "vcpu", 0, "vCPU limit")
				fs.Int64Var(&memory, "memory", 0, "memory limit in MiB")
				fs.Int64Var(&disk, "disk", 0, "disk limit in GiB")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<device-id>"); err != nil {
					return err
				}
				if vcpu <= 0 || memory <= 0 || disk <= 0 {
					return usagef("--vcpu, --memory and --disk are required and must be greater than zero")
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				d, err := c.UpdateDeviceLimits(a.ctx, args[0], &ownsandv1.DeviceLimits{VcpuCount: vcpu, MemoryMib: memory, DiskGib: disk}, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", d)
			}},
			"remove", command{usage: "<device-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<device-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.RemoveDevice(a.ctx, args[0], opt...))
			}},
		),
		"enrollment-token": mkGroup(
			"create", command{usage: "[--expires-in DURATION] [--device-name NAME]", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&expiresIn, "expires-in", "", "token lifetime, at most 24h (default: server default)")
				fs.StringVar(&name, "device-name", "", "name of the device that enrolls")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				var d time.Duration
				if expiresIn != "" {
					var err error
					if d, err = parseDuration(expiresIn); err != nil || d <= 0 {
						return usagef("invalid --expires-in %q", expiresIn)
					}
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				t, secret, err := c.CreateEnrollmentToken(a.ctx, d, name, opt...)
				if err != nil {
					return err
				}
				return a.printSecret(secret, "enrollment_token", t)
			}},
			"list", command{flags: withList(nil), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListEnrollmentTokens(a.ctx, listOpts(), opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"revoke", command{usage: "<enrollment-token-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<enrollment-token-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.RevokeEnrollmentToken(a.ctx, args[0], opt...))
			}},
		),
		"api-key": mkGroup(
			"create", command{usage: "<name>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<name>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				k, secret, err := c.CreateAPIKey(a.ctx, args[0], opt...)
				if err != nil {
					return err
				}
				return a.printSecret(secret, "api_key", k)
			}},
			"list", command{flags: withList(nil), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListAPIKeys(a.ctx, listOpts(), opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"revoke", command{usage: "<api-key-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<api-key-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.RevokeAPIKey(a.ctx, args[0], opt...))
			}},
		),
		"agent-version": mkGroup(
			"get", command{run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				p, err := c.GetAgentVersionPolicy(a.ctx, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", p)
			}},
			"set", command{usage: "--mode follow-default|pin|hold [--version V]", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&mode, "mode", "", "follow-default, pin or hold (required)")
				fs.StringVar(&version, "version", "", "pinned agent version (with --mode pin)")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				var m ownsandv1.AgentVersionPolicyMode
				switch mode {
				case "follow-default":
					m = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT
				case "pin":
					m = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN
					if version == "" {
						return usagef("--mode pin needs --version")
					}
				case "hold":
					m = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD
				default:
					return usagef("--mode must be follow-default, pin or hold")
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				p, err := c.UpdateAgentVersionPolicy(a.ctx, m, version, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", p)
			}},
		),
		"sandbox": mkGroup(
			"create", command{usage: "(--image linux|macos|windows | --snapshot ID | --template ID) --idle-ttl D --max-ttl D --vcpu N --memory MiB --disk GiB", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&image, "image", "", "base image: linux, macos or windows")
				fs.StringVar(&snapshot, "snapshot", "", "source snapshot ID")
				fs.StringVar(&template, "template", "", "source template ID")
				fs.StringVar(&idleTTL, "idle-ttl", "", "idle TTL, for example 30m (required)")
				fs.StringVar(&maxTTL, "max-ttl", "", "max TTL, at most 7d (required)")
				fs.Int64Var(&vcpu, "vcpu", 0, "vCPU count (required)")
				fs.Int64Var(&memory, "memory", 0, "memory in MiB (required)")
				fs.Int64Var(&disk, "disk", 0, "disk in GiB (required)")
				fs.StringVar(&deviceID, "device", "", "place on this device")
				fs.StringVar(&hostOS, "os", "", "host OS filter: linux, macos or windows")
				fs.StringVar(&arch, "arch", "", "architecture filter: amd64 or arm64")
				fs.StringVar(&network, "network", "", "network mode: outbound (default) or none")
				fs.Var(labels, "label", "label key=value (repeatable)")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				if len(args) != 0 {
					return usagef("sandbox create takes no arguments")
				}
				o, err := createOptions(image, snapshot, template, idleTTL, maxTTL, vcpu, memory, disk, deviceID, hostOS, arch, network, labels)
				if err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				sb, err := c.CreateSandbox(a.ctx, o, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", sb.Info())
			}},
			"get", command{usage: "<sandbox-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.sandboxCall(args, func(sb *ownsand.Sandbox) (proto.Message, error) { return sb.Refresh(a.ctx) })
			}},
			"list", command{flags: withList(func(fs *flag.FlagSet) {
				fs.StringVar(&deviceID, "device", "", "only sandboxes of this device")
			}), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListSandboxes(a.ctx, ownsand.ListSandboxesOptions{ListOptions: listOpts(), DeviceID: deviceID}, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"delete", command{usage: "<sandbox-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.sandboxCall(args, func(sb *ownsand.Sandbox) (proto.Message, error) { return sb.Delete(a.ctx) })
			}},
			"pause", command{usage: "<sandbox-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.sandboxCall(args, func(sb *ownsand.Sandbox) (proto.Message, error) { return sb.Pause(a.ctx) })
			}},
			"resume", command{usage: "<sandbox-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.sandboxCall(args, func(sb *ownsand.Sandbox) (proto.Message, error) { return sb.Resume(a.ctx) })
			}},
			"exec", execCommand(),
		),
		"file": mkGroup(
			"read", command{usage: "<sandbox-id> <guest-path> [--to PATH]", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&to, "to", "", "write the contents to this local path instead of standard output")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 2, "<sandbox-id> <guest-path>"); err != nil {
					return err
				}
				sb, err := a.sandbox(args[0])
				if err != nil {
					return err
				}
				defer sb.Close()
				var w io.Writer = a.io.out
				if to != "" {
					f, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
					if err != nil {
						return err
					}
					defer f.Close()
					w = f
				}
				if _, err := sb.ReadFile(a.ctx, args[1], w); err != nil {
					a.errorf("%v", err)
					return &exitCodeError{code: exitDataPath}
				}
				return nil
			}},
			"write", command{usage: "<sandbox-id> <guest-path> [--from PATH] [--mode 0644]", flags: func(fs *flag.FlagSet) {
				fs.StringVar(&from, "from", "", "read the contents from this local path instead of standard input")
				fs.StringVar(&fileMode, "mode", "0644", "file mode (octal)")
			}, run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 2, "<sandbox-id> <guest-path>"); err != nil {
					return err
				}
				m, err := strconv.ParseUint(fileMode, 8, 32)
				if err != nil {
					return usagef("invalid --mode %q", fileMode)
				}
				sb, err := a.sandbox(args[0])
				if err != nil {
					return err
				}
				defer sb.Close()
				r := a.io.in
				if from != "" {
					f, err := os.Open(from)
					if err != nil {
						return err
					}
					defer f.Close()
					r = f
				}
				if _, err := sb.WriteFile(a.ctx, args[1], r, uint32(m)); err != nil {
					a.errorf("%v", err)
					return &exitCodeError{code: exitDataPath}
				}
				return nil
			}},
		),
		"snapshot": mkGroup(
			"create", command{usage: "<sandbox-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.sandboxCall(args, func(sb *ownsand.Sandbox) (proto.Message, error) { return sb.Snapshot(a.ctx) })
			}},
			"get", command{usage: "<snapshot-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.idCall(args, "<snapshot-id>", func(c *ownsand.Client, id string, opt []ownsand.CallOption) (proto.Message, error) {
					return c.GetSnapshot(a.ctx, id, opt...)
				})
			}},
			"list", command{flags: withList(func(fs *flag.FlagSet) {
				fs.StringVar(&deviceID, "device", "", "only snapshots of this device")
			}), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListSnapshots(a.ctx, listOpts(), deviceID, opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"delete", command{usage: "<snapshot-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<snapshot-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.DeleteSnapshot(a.ctx, args[0], opt...))
			}},
			"export", command{usage: "<snapshot-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				return a.idCall(args, "<snapshot-id>", func(c *ownsand.Client, id string, opt []ownsand.CallOption) (proto.Message, error) {
					return c.ExportSnapshot(a.ctx, id, opt...)
				})
			}},
		),
		"template": mkGroup(
			"create", command{usage: "<snapshot-id> <name>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 2, "<snapshot-id> <name>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				t, err := c.CreateTemplate(a.ctx, args[0], args[1], opt...)
				if err != nil {
					return err
				}
				return a.printMessages(false, "", t)
			}},
			"list", command{flags: withList(nil), run: func(a *app, fs *flag.FlagSet, args []string) error {
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				items, next, err := c.ListTemplates(a.ctx, listOpts(), opt...)
				if err != nil {
					return err
				}
				return a.printMessages(true, next, msgs(items)...)
			}},
			"delete", command{usage: "<template-id>", run: func(a *app, fs *flag.FlagSet, args []string) error {
				if err := needArgs(args, 1, "<template-id>"); err != nil {
					return err
				}
				c, opt, err := a.orgClient()
				if err != nil {
					return err
				}
				return a.done(c.DeleteTemplate(a.ctx, args[0], opt...))
			}},
		),
	}
}

// done reports a command without output.
func (a *app) done(err error) error {
	if err != nil {
		return err
	}
	if a.fmt == formatJSON {
		_, err := fmt.Fprintln(a.io.out, "{}")
		return err
	}
	return nil
}

func (a *app) idCall(args []string, names string, f func(c *ownsand.Client, id string, opt []ownsand.CallOption) (proto.Message, error)) error {
	if err := needArgs(args, 1, names); err != nil {
		return err
	}
	c, opt, err := a.orgClient()
	if err != nil {
		return err
	}
	m, err := f(c, args[0], opt)
	if err != nil {
		return err
	}
	return a.printMessages(false, "", m)
}

func (a *app) sandboxCall(args []string, f func(sb *ownsand.Sandbox) (proto.Message, error)) error {
	if err := needArgs(args, 1, "<sandbox-id>"); err != nil {
		return err
	}
	sb, err := a.sandbox(args[0])
	if err != nil {
		return err
	}
	defer sb.Close()
	m, err := f(sb)
	if err != nil {
		return err
	}
	return a.printMessages(false, "", m)
}

// parseDuration accepts Go durations and a "d" (24-hour day) suffix.
func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func createOptions(image, snapshot, template, idleTTL, maxTTL string, vcpu, memory, disk int64, device, hostOS, arch, network string, labels map[string]string) (ownsand.CreateSandboxOptions, error) {
	o := ownsand.CreateSandboxOptions{SnapshotID: snapshot, TemplateID: template, VCPUCount: vcpu, MemoryMiB: memory, DiskGiB: disk, DeviceID: device, Labels: labels}
	sources := 0
	for _, s := range []string{image, snapshot, template} {
		if s != "" {
			sources++
		}
	}
	if sources != 1 {
		return o, usagef("exactly one of --image, --snapshot and --template is required")
	}
	switch image {
	case "":
	case "linux":
		o.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_LINUX
	case "macos":
		o.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_MACOS
	case "windows":
		o.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_WINDOWS
	default:
		return o, usagef("--image must be linux, macos or windows")
	}
	if idleTTL == "" || maxTTL == "" {
		return o, usagef("--idle-ttl and --max-ttl are required; there is no default TTL")
	}
	var err error
	if o.IdleTTL, err = parseDuration(idleTTL); err != nil {
		return o, usagef("invalid --idle-ttl %q", idleTTL)
	}
	if o.MaxTTL, err = parseDuration(maxTTL); err != nil {
		return o, usagef("invalid --max-ttl %q", maxTTL)
	}
	if vcpu <= 0 || memory <= 0 || disk <= 0 {
		return o, usagef("--vcpu, --memory and --disk are required and must be greater than zero")
	}
	switch hostOS {
	case "":
	case "linux":
		o.HostOS = ownsandv1.HostOs_HOST_OS_LINUX
	case "macos":
		o.HostOS = ownsandv1.HostOs_HOST_OS_MACOS
	case "windows":
		o.HostOS = ownsandv1.HostOs_HOST_OS_WINDOWS
	default:
		return o, usagef("--os must be linux, macos or windows")
	}
	switch arch {
	case "":
	case "amd64":
		o.Arch = ownsandv1.Arch_ARCH_AMD64
	case "arm64":
		o.Arch = ownsandv1.Arch_ARCH_ARM64
	default:
		return o, usagef("--arch must be amd64 or arm64")
	}
	switch network {
	case "":
	case "outbound":
		o.Network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	case "none":
		o.Network = ownsandv1.NetworkMode_NETWORK_MODE_NONE
	default:
		return o, usagef("--network must be outbound or none")
	}
	if err := o.Validate(); err != nil {
		return o, usagef("%v", err)
	}
	return o, nil
}

// readSecret reads a secret from a file or from standard input and trims
// the line ending.
func (a *app) readSecret(path string) (string, error) {
	var r io.Reader = a.io.in
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", usagef("no invitation secret on standard input or in --secret-file")
	}
	return s, nil
}
